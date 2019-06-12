package exchange

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/robfig/cron"
	"github.com/sero-cash/go-czero-import/cpt"
	"github.com/sero-cash/go-czero-import/keys"
	"github.com/sero-cash/go-sero/accounts"
	"github.com/sero-cash/go-sero/common"
	"github.com/sero-cash/go-sero/common/base58"
	"github.com/sero-cash/go-sero/common/math"
	"github.com/sero-cash/go-sero/core"
	"github.com/sero-cash/go-sero/core/types"
	"github.com/sero-cash/go-sero/event"
	"github.com/sero-cash/go-sero/log"
	"github.com/sero-cash/go-sero/rlp"
	"github.com/sero-cash/go-sero/serodb"
	"github.com/sero-cash/go-sero/zero/light"
	"github.com/sero-cash/go-sero/zero/light/light_ref"
	"github.com/sero-cash/go-sero/zero/light/light_types"
	"github.com/sero-cash/go-sero/zero/localdb"
	"github.com/sero-cash/go-sero/zero/txs/assets"
	"github.com/sero-cash/go-sero/zero/txs/stx"
	"github.com/sero-cash/go-sero/zero/utils"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type Account struct {
	wallet  accounts.Wallet
	pk      *keys.Uint512
	tk      *keys.Uint512
	sk      *keys.PKr
	skr     keys.PKr
	mainPkr keys.PKr
}

type PkrAccount struct {
	Pkr      keys.PKr
	balances map[string]*big.Int
	num      uint64
}

type Uxto struct {
	Root   keys.Uint256
	TxHash keys.Uint256
	Nil    keys.Uint256
	Num    uint64
	Asset  assets.Asset
	flag   int
}

type UxtoList []Uxto

func (list UxtoList) Len() int {
	return len(list)
}

func (list UxtoList) Swap(i, j int) {
	list[i], list[j] = list[j], list[i]
}

func (list UxtoList) Less(i, j int) bool {
	if list[i].flag == list[j].flag {
		return list[i].Asset.Tkn.Value.ToIntRef().Cmp(list[j].Asset.Tkn.Value.ToIntRef()) < 0
	} else {
		return list[i].flag < list[j].flag
	}
}

type Reception struct {
	Addr     keys.PKr
	Currency string
	Value    *big.Int
}

type TxParam struct {
	From       keys.Uint512
	Receptions []Reception
	Gas        uint64
	GasPrice   uint64
	Roots      []keys.Uint256
}

type (
	HandleUxtoFunc func(uxto Uxto)
)

type PkKey struct {
	Pkr keys.PKr
	Num uint64
}

type PkrKey struct {
	account *Account
	pkr     keys.PKr

	num uint64
}

type FetchJob struct {
	start    uint64
	accounts []Account
}

type Exchange struct {
	db             *serodb.LDBDatabase
	txPool         *core.TxPool
	accountManager *accounts.Manager

	accounts    map[keys.Uint512]*Account
	pkrAccounts sync.Map

	sri light.SRI
	sli light.SLI

	usedFlag sync.Map
	inits    sync.Map

	jobs []FetchJob

	//numbers map[keys.Uint512]uint64
	numbers sync.Map

	feed    event.Feed
	updater event.Subscription        // Wallet update subscriptions for all backends
	update  chan accounts.WalletEvent // Subscription sink for backend wallet changes
	quit    chan chan error
	lock    sync.RWMutex
}

func NewExchange(dbpath string, txPool *core.TxPool, accountManager *accounts.Manager, autoMerge bool) (exchange *Exchange) {

	update := make(chan accounts.WalletEvent, 1)
	updater := accountManager.Subscribe(update)

	exchange = &Exchange{
		txPool:         txPool,
		accountManager: accountManager,
		sri:            light.SRI_Inst,
		sli:            light.SLI_Inst,
		update:         update,
		updater:        updater,
	}

	db, err := serodb.NewLDBDatabase(dbpath, 1024, 1024)
	if err != nil {
		panic(err)
	}
	exchange.db = db

	exchange.numbers = sync.Map{}
	exchange.accounts = map[keys.Uint512]*Account{}
	for _, w := range accountManager.Wallets() {
		exchange.initWallet(w)
	}

	iterator := exchange.db.NewIteratorWithPrefix(numPrefix)
	for iterator.Next() {
		key := iterator.Key()
		num := decodeNumber(iterator.Value())
		var pk keys.Uint512
		copy(pk[:], key[3:])
		exchange.numbers.Store(pk, num)
	}

	exchange.pkrAccounts = sync.Map{}
	exchange.usedFlag = sync.Map{}
	exchange.inits = sync.Map{}

	AddJob("0/10 * * * * ?", exchange.fetchBlockInfo)

	if autoMerge {
		AddJob("0 0/1 * * * ?", exchange.merge)
	}

	go exchange.updateAccount()
	log.Info("Init NewExchange success")
	return
}

func (self *Exchange) initWallet(w accounts.Wallet) {
	account := Account{}
	account.wallet = w
	account.pk = w.Accounts()[0].Address.ToUint512()
	account.tk = w.Accounts()[0].Tk.ToUint512()
	copy(account.skr[:], account.tk[:])
	account.mainPkr = self.createPkr(account.pk, 1)
	self.accounts[*account.pk] = &account
	self.numbers.Store(*account.pk, uint64(1))
	log.Info("PK", "address", w.Accounts()[0].Address)
}

func (self *Exchange) updateAccount() {
	// Close all subscriptions when the manager terminates
	defer func() {
		self.lock.Lock()
		self.updater.Unsubscribe()
		self.updater = nil
		self.lock.Unlock()
	}()

	// Loop until termination
	for {
		select {
		case event := <-self.update:
			// Wallet event arrived, update local cache
			self.lock.Lock()
			switch event.Kind {
			case accounts.WalletArrived:
				//wallet := event.Wallet
				self.initWallet(event.Wallet)
			case accounts.WalletDropped:
				pk := *event.Wallet.Accounts()[0].Address.ToUint512()
				self.numbers.Delete(pk)
			}
			self.lock.Unlock()

		case errc := <-self.quit:
			// Manager terminating, return
			errc <- nil
			return
		}
	}
}

func (self *Exchange) GetPkr(pk keys.Uint512, index uint64) (pkr keys.PKr, err error) {
	if index < 100 {
		return pkr, errors.New("index must > 100")
	}
	if _, ok := self.accounts[pk]; !ok {
		return pkr, errors.New("not found Pk")
	}

	return self.createPkr(&pk, index), nil
}

func (self *Exchange) GetBalances(pkr keys.PKr) (balances map[string]*big.Int) {
	pk := pkr.ToUint512()
	if account, ok := self.accounts[pk]; ok {
		prefix := append(pkPrefix, account.pk[:]...)
		iterator := self.db.NewIteratorWithPrefix(prefix)

		balances = map[string]*big.Int{}
		for iterator.Next() {
			key := iterator.Key()
			var root keys.Uint256
			copy(root[:], key[98:130])

			if uxto, err := self.getUxto(root); err == nil {
				if uxto.Asset.Tkn != nil {
					currency := common.BytesToString(uxto.Asset.Tkn.Currency[:])
					if amount, ok := balances[currency]; ok {
						amount.Add(amount, uxto.Asset.Tkn.Value.ToIntRef())
					} else {
						balances[currency] = new(big.Int).Set(uxto.Asset.Tkn.Value.ToIntRef())
					}
				}
			}
		}
		return
	} else {
		if _, ok := self.inits.LoadOrStore(pkr, 1); !ok {
			self.initAccount(pkr)
			self.inits.Delete(pkr)
		}
		if value, ok := self.pkrAccounts.Load(pkr); ok {
			return value.(*PkrAccount).balances
		}
	}

	return map[string]*big.Int{}
}

func (self *Exchange) GetRecords(pkr keys.PKr, begin, end uint64) (records []Uxto, err error) {
	if _, ok := self.isMyPkr(pkr); ok {
		err = self.iteratorUxto(pkr, begin, end, func(uxto Uxto) {
			records = append(records, uxto)
		})
	}
	return
}

func (self *Exchange) GenTx(param TxParam) (txParam *light_types.GenTxParam, e error) {
	uxtos, err := self.preGenTx(param)
	if err != nil {
		return nil, err
	}

	if _, ok := self.accounts[param.From]; !ok {
		return nil, errors.New("not found Pk")
	}

	txParam, e = self.buildTxParam(uxtos, self.accounts[param.From], param.Receptions, param.Gas, param.GasPrice)
	return
}

func (self *Exchange) GenTxWithSign(param TxParam) (*light_types.GTx, error) {
	uxtos, err := self.preGenTx(param)
	if err != nil {
		return nil, err
	}

	var account *Account
	if _, ok := self.accounts[param.From]; !ok {
		return nil, errors.New("not found Pk")
	} else {
		account = self.accounts[param.From]
	}

	gtx, err := self.genTx(uxtos, account, param.Receptions, param.Gas, param.GasPrice)
	if err != nil {
		log.Error("Exchange genTx", "error", err)
		return nil, err
	}
	gtx.Hash = gtx.Tx.ToHash()
	log.Info("Exchange genTx success")
	return gtx, nil
}

func (self *Exchange) preGenTx(param TxParam) (uxtos []Uxto, err error) {
	var roots []keys.Uint256
	if len(param.Roots) > 0 {
		roots = param.Roots
		for _, root := range roots {
			uxto, err := self.getUxto(root)
			if err != nil {
				return uxtos, err
			}
			uxtos = append(uxtos, uxto)
		}
	} else {
		amounts := map[string]*big.Int{}
		for _, each := range param.Receptions {
			if amount, ok := amounts[each.Currency]; ok {
				amount.Add(amount, each.Value)
			} else {
				amounts[each.Currency] = new(big.Int).Set(each.Value)
			}
		}
		if amount, ok := amounts["SERO"]; ok {
			amount.Add(amount, new(big.Int).Mul(new(big.Int).SetUint64(param.Gas), new(big.Int).SetUint64(param.GasPrice)))
		} else {
			amount = new(big.Int).Mul(new(big.Int).SetUint64(param.Gas), new(big.Int).SetUint64(param.GasPrice))
		}
		for currency, amount := range amounts {
			if list, err := self.findUxtos(&param.From, currency, amount); err != nil {
				return uxtos, err
			} else {
				uxtos = append(uxtos, list...)
			}
		}
	}
	return
}

//func (self *Exchange) CommitTx(gtx light_types.GTx) (err error) {
//	return self.commitTx(&gtx)
//}

func (self *Exchange) isPk(addr keys.PKr) bool {
	byte32 := common.Hash{}
	return bytes.Equal(byte32[:], addr[64:96])
}

func (self *Exchange) createPkr(pk *keys.Uint512, index uint64) keys.PKr {
	r := keys.Uint256{}
	copy(r[:], common.LeftPadBytes(encodeNumber(index), 32))
	return keys.Addr2PKr(pk, &r)
}

func (self *Exchange) genTx(uxtos []Uxto, account *Account, receptions []Reception, gas, gasPrice uint64) (*light_types.GTx, error) {
	txParam, err := self.buildTxParam(uxtos, account, receptions, gas, gasPrice)
	if err != nil {
		return nil, err
	}

	if account.sk == nil {
		seed, err := account.wallet.GetSeed()
		if err != nil {
			return nil, err
		}
		sk := keys.Seed2Sk(seed.SeedToUint256())
		account.sk = new(keys.PKr)
		copy(account.sk[:], sk[:])
	}

	txParam.From.SKr = *account.sk
	for index, _ := range txParam.Ins {
		txParam.Ins[index].SKr = *account.sk
	}

	gtx, err := self.sli.GenTx(txParam)
	if err != nil {
		return nil, err
	}
	return &gtx, nil
}

func (self *Exchange) buildTxParam(uxtos []Uxto, account *Account, receptions []Reception, gas, gasPrice uint64) (txParam *light_types.GenTxParam, e error) {
	txParam = new(light_types.GenTxParam)
	txParam.Gas = gas
	txParam.GasPrice = *big.NewInt(int64(gasPrice))

	txParam.From = light_types.Kr{PKr: account.mainPkr}

	roots := []keys.Uint256{}
	for _, uxtos := range uxtos {
		roots = append(roots, uxtos.Root)
	}
	Ins := []light_types.GIn{}
	wits, err := self.sri.GetAnchor(roots)
	if err != nil {
		e = err
		return
	}

	amounts := make(map[string]*big.Int)
	ticekts := make(map[keys.Uint256]keys.Uint256)
	for index, uxto := range uxtos {
		if out := localdb.GetRoot(light_ref.Ref_inst.Bc.GetDB(), &uxto.Root); out != nil {
			Ins = append(Ins, light_types.GIn{Out: light_types.Out{Root: uxto.Root, State: *out}, Witness: wits[index]})

			if uxto.Asset.Tkn != nil {
				currency := strings.Trim(string(uxto.Asset.Tkn.Currency[:]), string([]byte{0}))
				if amount, ok := amounts[currency]; ok {
					amount.Add(amount, uxto.Asset.Tkn.Value.ToIntRef())
				} else {
					amounts[currency] = new(big.Int).Set(uxto.Asset.Tkn.Value.ToIntRef())
				}

			}
			if uxto.Asset.Tkt != nil {
				ticekts[uxto.Asset.Tkt.Value] = uxto.Asset.Tkt.Category
			}
		}
	}

	Outs := []light_types.GOut{}
	for _, reception := range receptions {
		currency := strings.ToUpper(reception.Currency)
		if amount, ok := amounts[currency]; ok && amount.Cmp(reception.Value) >= 0 {

			if self.isPk(reception.Addr) {
				pk := reception.Addr.ToUint512()
				pkr := self.createPkr(&pk, 1)
				Outs = append(Outs, light_types.GOut{PKr: pkr, Asset: assets.Asset{Tkn: &assets.Token{
					Currency: *common.BytesToHash(common.LeftPadBytes([]byte(currency), 32)).HashToUint256(),
					Value:    utils.U256(*reception.Value),
				}}})
			} else {
				Outs = append(Outs, light_types.GOut{PKr: reception.Addr, Asset: assets.Asset{Tkn: &assets.Token{
					Currency: *common.BytesToHash(common.LeftPadBytes([]byte(currency), 32)).HashToUint256(),
					Value:    utils.U256(*reception.Value),
				}}})
			}

			amount.Sub(amount, reception.Value)
			if amount.Sign() == 0 {
				delete(amounts, currency)
			}
		}

	}

	fee := new(big.Int).Mul(new(big.Int).SetUint64(gas), new(big.Int).SetUint64(gasPrice))
	if amount, ok := amounts["SERO"]; !ok || amount.Cmp(fee) < 0 {
		e = fmt.Errorf("SSI GenTx Error: not enough")
		return
	} else {
		amount.Sub(amount, fee)
		if amount.Sign() == 0 {
			delete(amounts, "SERO")
		}
	}

	if len(amounts) > 0 {
		for currency, value := range amounts {
			Outs = append(Outs, light_types.GOut{PKr: account.mainPkr, Asset: assets.Asset{Tkn: &assets.Token{
				Currency: *common.BytesToHash(common.LeftPadBytes([]byte(currency), 32)).HashToUint256(),
				Value:    utils.U256(*value),
			}}})
		}
	}
	if len(ticekts) > 0 {
		for value, category := range ticekts {
			Outs = append(Outs, light_types.GOut{PKr: account.mainPkr, Asset: assets.Asset{Tkt: &assets.Ticket{
				Category: category,
				Value:    value,
			}}})
		}
	}

	txParam.Ins = Ins
	txParam.Outs = Outs

	for _, uxto := range uxtos {
		self.usedFlag.Store(uxto.Nil, 1)
	}

	return
}

func (self *Exchange) commitTx(tx *light_types.GTx) (err error) {
	gasPrice := big.Int(tx.GasPrice)
	gas := uint64(tx.Gas)
	signedTx := types.NewTxWithGTx(gas, &gasPrice, &tx.Tx)
	log.Info("Exchange commitTx", "txhash", signedTx.Hash().String())
	err = self.txPool.AddLocal(signedTx)
	return err
}

func (self *Exchange) initAccount(pkr keys.PKr) (err error) {
	if _, ok := self.isMyPkr(pkr); !ok {
		return
	}

	var account *PkrAccount
	if value, ok := self.pkrAccounts.Load(pkr); ok {
		account = value.(*PkrAccount)

	} else {
		account = &PkrAccount{}
		account.Pkr = pkr
		account.balances = map[string]*big.Int{}
		self.pkrAccounts.Store(pkr, account)
	}

	err = self.iteratorUxto(pkr, account.num+1, math.MaxUint64, func(uxto Uxto) {
		if uxto.Asset.Tkn != nil {
			curency := strings.ToUpper(common.BytesToString(uxto.Asset.Tkn.Currency[:]))
			if balance, ok := account.balances[curency]; ok {
				balance.Add(balance, uxto.Asset.Tkn.Value.ToIntRef())
			} else {
				account.balances[curency] = new(big.Int).Set(uxto.Asset.Tkn.Value.ToIntRef())
			}
			account.num = uxto.Num
		}
	})

	return
}

func (self *Exchange) iteratorUxto(pkr keys.PKr, begin, end uint64, handler HandleUxtoFunc) (e error) {
	iterator := self.db.NewIteratorWithPrefix(append(pkrPrefix, pkr[:]...))
	for ok := iterator.Seek(uxtoPkrKey(pkr, begin)); ok; ok = iterator.Next() {
		key := iterator.Key()
		num := decodeNumber(key[99:107])
		if num > end {
			break
		}

		value := iterator.Value()
		roots := []keys.Uint256{}
		if err := rlp.Decode(bytes.NewReader(value), &roots); err != nil {
			log.Error("Invalid roots RLP", "pkr", common.Bytes2Hex(pkr[:]), "blockNumber", num, "err", err)
			e = err
			return
		}
		for _, root := range roots {
			if uxto, err := self.getUxto(root); err != nil {
				return
			} else {
				handler(uxto)
			}
		}
	}

	return
}

func (self *Exchange) getUxto(root keys.Uint256) (uxto Uxto, e error) {
	data, err := self.db.Get(rootKey(root))
	if err != nil {
		return
	}
	if err := rlp.Decode(bytes.NewReader(data), &uxto); err != nil {
		log.Error("Exchange Invalid uxto RLP", "root", common.Bytes2Hex(root[:]), "err", err)
		e = err
		return
	}

	if value, ok := self.usedFlag.Load(uxto.Nil); ok {
		uxto.flag = value.(int)
	}
	return
}

func (self *Exchange) findUxtos(pk *keys.Uint512, currency string, amount *big.Int) (uxtos []Uxto, e error) {
	currency = strings.ToUpper(currency)
	prefix := append(pkPrefix, append(pk[:], common.LeftPadBytes([]byte(currency), 32)...)...)
	iterator := self.db.NewIteratorWithPrefix(prefix)

	list := UxtoList{}
	for iterator.Next() {
		key := iterator.Key()
		var root keys.Uint256
		copy(root[:], key[98:130])

		if uxto, err := self.getUxto(root); err == nil {
			if _, ok := self.usedFlag.Load(uxto.Nil); !ok {
				uxtos = append(uxtos, uxto)
				amount.Sub(amount, uxto.Asset.Tkn.Value.ToIntRef())
			} else {
				list = append(list, uxto)
			}
		}
		if amount.Sign() <= 0 {
			break
		}
	}

	if amount.Sign() > 0 {
		if list.Len() > 0 {
			sort.Sort(list)
			for _, uxto := range list {
				uxtos = append(uxtos, uxto)
				amount.Sub(amount, uxto.Asset.Tkn.Value.ToIntRef())
				if amount.Sign() <= 0 {
					break
				}
			}
		}
	}

	if amount.Sign() > 0 {
		e = errors.New("not enough token")
	}
	return
}

func DecOuts(outs []light_types.Out, skr *keys.PKr) (douts []light_types.DOut) {
	sk := keys.Uint512{}
	copy(sk[:], skr[:])
	for _, out := range outs {
		dout := light_types.DOut{}

		if out.State.OS.Out_O != nil {
			dout.Asset = out.State.OS.Out_O.Asset.Clone()
			dout.Memo = out.State.OS.Out_O.Memo
			dout.Nil = cpt.GenTil(&sk, out.State.OS.RootCM)
		} else {
			key, flag := keys.FetchKey(&sk, &out.State.OS.Out_Z.RPK)
			info_desc := cpt.InfoDesc{}
			info_desc.Key = key
			info_desc.Flag = flag
			info_desc.Einfo = out.State.OS.Out_Z.EInfo
			cpt.DecOutput(&info_desc)

			if e := stx.ConfirmOut_Z(&info_desc, out.State.OS.Out_Z); e == nil {
				dout.Asset = assets.NewAsset(
					&assets.Token{
						info_desc.Tkn_currency,
						utils.NewU256_ByKey(&info_desc.Tkn_value),
					},
					&assets.Ticket{
						info_desc.Tkt_category,
						info_desc.Tkt_value,
					},
				)
				dout.Memo = info_desc.Memo

				dout.Nil = cpt.GenTil(&sk, out.State.OS.RootCM)
			}
		}
		douts = append(douts, dout)
	}
	return
}

func (self *Exchange) fetchBlockInfo() {

	indexs := map[uint64][]keys.Uint512{}
	self.numbers.Range(func(key, value interface{}) bool {
		pk := key.(keys.Uint512)
		num := value.(uint64)
		if list, ok := indexs[num]; ok {
			indexs[num] = append(list, pk)
		} else {
			indexs[num] = []keys.Uint512{pk}
		}
		return true
	})

	for num, pks := range indexs {
		list := []string{}
		for _, pk := range pks {
			list = append(list, base58.EncodeToString(pk[:]))
		}
		for {
			log.Info("Exchange fetchAndIndexUxto", "num", num, "pks", list)
			if self.fetchAndIndexUxto(num, pks) < 1000 {
				break
			}
		}
	}
}

func (self *Exchange) fetchAndIndexUxto(start uint64, pks []keys.Uint512) (count int) {

	blocks, err := self.sri.GetBlocksInfo(start, 1000)
	if err != nil {
		log.Info("Exchange GetBlocksInfo", "error", err)
		return
	}

	if len(blocks) == 0 {
		return
	}

	outs := map[PkrKey][]light_types.Out{}
	nils := []keys.Uint256{}
	num := start
	for _, block := range blocks {
		for _, out := range block.Outs {
			var pkr keys.PKr

			if out.State.OS.Out_Z != nil {
				pkr = out.State.OS.Out_Z.PKr
			}
			if out.State.OS.Out_O != nil {
				pkr = out.State.OS.Out_O.Addr
			}

			key := PkrKey{pkr: pkr, num: out.State.Num}

			if account, ok := self.ownPkr(pks, pkr); ok {
				key.account = account
			} else {
				continue
			}

			if _, ok := outs[key]; ok {
				outs[key] = append(outs[key], out)
			} else {
				outs[key] = []light_types.Out{out}
			}

		}
		if len(block.Nils) > 0 {
			nils = append(nils, block.Nils...)
		}
	}

	uxtos := map[PkrKey][]Uxto{}
	for key, outs := range outs {
		douts := DecOuts(outs, &key.account.skr)
		list := []Uxto{}
		for index, out := range douts {
			dout := outs[index]
			list = append(list, Uxto{Root: dout.Root, Nil: out.Nil, TxHash: dout.State.TxHash, Num: dout.State.Num, Asset: out.Asset})
		}
		uxtos[key] = list
	}

	batch := self.db.NewBatch()
	if len(uxtos) > 0 || len(nils) > 0 {
		if err := self.indexBlocks(batch, uxtos, nils); err != nil {
			log.Error("indexBlocks ", "error", err)
		}
	}

	count = len(blocks)
	num = uint64(blocks[count-1].Num) + 1
	// "NUM"+pk  => num
	data := encodeNumber(num)
	for _, pk := range pks {
		batch.Put(numKey(pk), data)
		self.numbers.Store(pk, num)
	}

	batch.Write()
	return
}

func (self *Exchange) indexBlocks(batch serodb.Batch, uxtos map[PkrKey][]Uxto, nils []keys.Uint256) (err error) {
	ops := map[string]string{}
	for key, list := range uxtos {
		roots := []keys.Uint256{}
		for _, uxto := range list {
			data, err := rlp.EncodeToBytes(uxto)
			if err != nil {
				return err
			}

			// "ROOT" + root
			batch.Put(rootKey(uxto.Root), data)

			var pkKey []byte
			if uxto.Asset.Tkn != nil {
				// "PK" + pk + currency + root
				pkKey = uxtoPkKey(*key.account.pk, uxto.Asset.Tkn.Currency[:], &uxto.Root)

			} else if uxto.Asset.Tkt != nil {
				// "PK" + pk + tkt + root
				pkKey = uxtoPkKey(*key.account.pk, uxto.Asset.Tkt.Value[:], &uxto.Root)
			}
			// "PK" + pk + currency + root => 0
			ops[common.Bytes2Hex(pkKey)] = common.Bytes2Hex([]byte{0})

			// "NIL" + pk + tkt + root => "PK" + pk + currency + root
			nilkey := nilKey(uxto.Nil)
			rootkey := nilKey(uxto.Root)

			// "NIL" +nil/root => pkKey
			ops[common.Bytes2Hex(nilkey)] = common.Bytes2Hex(pkKey)
			ops[common.Bytes2Hex(rootkey)] = common.Bytes2Hex(pkKey)

			roots = append(roots, uxto.Root)
			log.Info("Index add", "PK", base58.EncodeToString(key.account.pk[:]), "Nil", common.Bytes2Hex(uxto.Nil[:]), "Key", common.Bytes2Hex(pkKey[:]), "Value", uxto.Asset.Tkn.Value)
		}

		data, err := rlp.EncodeToBytes(roots)
		if err != nil {
			return err
		}
		// "PKR" + prk + blockNumber => [roots]
		batch.Put(uxtoPkrKey(key.pkr, key.num), data)
	}

	for _, Nil := range nils {

		key := nilKey(Nil)
		hex := common.Bytes2Hex(key)
		if value, ok := ops[hex]; ok {
			delete(ops, hex)
			delete(ops, value)
			log.Info("Index del", "nil", common.Bytes2Hex(Nil[:]), "key", value)
		} else {
			data, _ := self.db.Get(key)
			if data != nil {
				batch.Delete(data)
				batch.Delete(nilKey(Nil))
				log.Info("Index del", "nil", common.Bytes2Hex(Nil[:]), "key", common.Bytes2Hex(data))
			}
		}
		self.usedFlag.Delete(common.Bytes2Hex(Nil[:]))
	}

	for key, value := range ops {
		batch.Put(common.Hex2Bytes(key), common.Hex2Bytes(value))
	}

	return nil
}

func (self *Exchange) ownPkr(pks []keys.Uint512, pkr keys.PKr) (account *Account, ok bool) {
	for _, pk := range pks {
		account = self.accounts[pk]
		if account == nil {
			log.Warn("error")
			continue
		}
		if keys.IsMyPKr(account.tk, &pkr) {
			return account, true
		}
	}
	return
}

func (self *Exchange) isMyPkr(pkr keys.PKr) (account *Account, ok bool) {
	for _, account := range self.accounts {
		if keys.IsMyPKr(account.tk, &pkr) {
			return account, true
		}
	}
	return nil, false
}

func (self *Exchange) merge() {
	for key, account := range self.accounts {
		prefix := uxtoPkKey(key, common.LeftPadBytes([]byte("SERO"), 32), nil)
		iterator := self.db.NewIteratorWithPrefix(prefix)
		uxtos := UxtoList{}
		for iterator.Next() {
			key := iterator.Key()
			var root keys.Uint256
			copy(root[:], key[98:130])

			if uxto, err := self.getUxto(root); err == nil {
				uxtos = append(uxtos, uxto)
			}

			if uxtos.Len() > 150 {
				break
			}
		}
		if uxtos.Len() <= 10 {
			continue
		}

		sort.Sort(uxtos)

		uxtos = uxtos[0 : uxtos.Len()-8]

		if uxtos.Len() > 1 {
			amount := new(big.Int)
			for _, uxto := range uxtos {
				amount.Add(amount, uxto.Asset.Tkn.Value.ToIntRef())
			}
			amount.Sub(amount, new(big.Int).Mul(big.NewInt(25000), big.NewInt(1000000000)))
			gtx, err := self.genTx(uxtos, account, []Reception{{Value: amount, Currency: "SERO", Addr: account.mainPkr}}, 25000, 1000000000)
			if err != nil {
				log.Error("Exchange merge uxto", "error", err)
				continue
			}
			self.commitTx(gtx)
		}
	}

}

var (
	numPrefix  = []byte("NUM")
	pkPrefix   = []byte("PK")
	pkrPrefix  = []byte("PKR")
	rootPrefix = []byte("ROOT")
	nilPrefix  = []byte("NIL")

	Prefix = []byte("Out")
)

func numKey(pk keys.Uint512) []byte {
	return append(numPrefix, pk[:]...)
}

func nilKey(nil keys.Uint256) []byte {
	return append(nilPrefix, nil[:]...)
}

func rootKey(root keys.Uint256) []byte {
	return append(rootPrefix, root[:]...)
}

// uxtoKey = pk + currency +root
func uxtoPkKey(pk keys.Uint512, currency []byte, root *keys.Uint256) []byte {
	key := append(pkPrefix, pk[:]...)
	if len(currency) > 0 {
		key = append(key, currency...)
	}
	if root != nil {
		key = append(key, root[:]...)
	}
	return key
}

func uxtoPkrKey(pkr keys.PKr, number uint64) []byte {
	return append(pkrPrefix, append(pkr[:], encodeNumber(number)...)...)
}

func encodeNumber(number uint64) []byte {
	enc := make([]byte, 8)
	binary.BigEndian.PutUint64(enc, number)
	return enc
}

func decodeNumber(data []byte) uint64 {
	return binary.BigEndian.Uint64(data)
}

func AddJob(spec string, run RunFunc) (*cron.Cron) {
	c := cron.New()
	c.AddJob(spec, &RunJob{run: run})
	c.Start()
	return c
}

type (
	RunFunc func()
)

type RunJob struct {
	runing int32
	run    RunFunc
}

func (r *RunJob) Run() {
	x := atomic.LoadInt32(&r.runing)
	if x == 1 {
		return
	}

	atomic.StoreInt32(&r.runing, 1)
	defer func() {
		atomic.StoreInt32(&r.runing, 0)
	}()

	r.run()
}