package ethapi

import (
	"context"

	"github.com/pkg/errors"

	"github.com/sero-cash/go-sero/common/hexutil"
	"github.com/sero-cash/go-sero/core/types"

	"github.com/sero-cash/go-sero/common"

	"github.com/sero-cash/go-czero-import/keys"

	"github.com/sero-cash/go-sero/core/rawdb"
	"github.com/sero-cash/go-sero/zero/txs/assets"
	"github.com/sero-cash/go-sero/zero/txtool/flight"

	"github.com/sero-cash/go-sero/zero/txtool"
)

type PublicFlightAPI struct {
	exchange *PublicExchangeAPI
}

func (s *PublicFlightAPI) GetBlocksInfo(ctx context.Context, start uint64, count uint64) ([]txtool.Block, error) {
	block, err := flight.SRI_Inst.GetBlocksInfoByDelay(start, count, 0)
	if err != nil {
		return nil, err
	}
	return block, err
}

func (s *PublicFlightAPI) GetBlockByNumber(ctx context.Context, blockNum *int64) (map[string]interface{}, error) {
	return s.exchange.GetBlockByNumber(ctx, blockNum)
}

type GOutArgs struct {
	PKr   PKrAddress
	Asset assets.Asset
	Memo  keys.Uint512
}

func (self *GOutArgs) ToOut() (ret txtool.GOut) {
	ret.PKr = *self.PKr.ToPKr()
	ret.Asset = self.Asset
	ret.Memo = self.Memo
	return
}

type PreTxParamArgs struct {
	Gas      uint64
	GasPrice uint64
	From     PKrAddress
	Ins      []keys.Uint256
	Outs     []GOutArgs
}

func (self *PreTxParamArgs) ToParam() (ret flight.PreTxParam) {
	ret.Gas = self.Gas
	ret.GasPrice = self.GasPrice
	ret.From = *self.From.ToPKr()
	ret.Ins = self.Ins
	for _, out := range self.Outs {
		ret.Outs = append(ret.Outs, out.ToOut())
	}
	return
}

func (s *PublicFlightAPI) GenTxParam(ctx context.Context, param PreTxParamArgs, tk TKAddress) (p txtool.GTxParam, e error) {
	preTxParam := param.ToParam()
	return flight.GenTxParam(&preTxParam, tk.ToUint512())
}

func (s *PublicFlightAPI) CommitTx(ctx context.Context, args *txtool.GTx) error {
	return s.exchange.CommitTx(ctx, args)
}

func (s *PublicFlightAPI) Trace2Root(ctx context.Context, tk TKAddress, trace keys.Uint256, base keys.Uint256) (root keys.Uint256, e error) {
	if r := flight.Trace2Root(tk.ToUint512().NewRef(), &trace, &base); r != nil {
		root = *r
		return
	} else {
		e = errors.New("can not find the root")
		return
	}
}

func (s *PublicFlightAPI) GetOut(ctx context.Context, root keys.Uint256) (out *txtool.Out, e error) {
	rt := flight.GetOut(&root, 0)
	if rt == nil {
		return
	} else {
		out = &txtool.Out{}
		out.Root = root
		out.State = *rt
		return
	}
}

func (s *PublicFlightAPI) GetTx(ctx context.Context, txhash keys.Uint256) (gtx txtool.GTx, e error) {
	hash := common.Hash{}
	copy(hash[:], txhash[:])

	var tx *types.Transaction
	tx, _, _, _ = rawdb.ReadTransaction(s.exchange.b.ChainDb(), hash)
	if tx == nil {
		tx = s.exchange.b.GetPoolTransaction(hash)
	}
	if tx != nil {
		gtx.Hash = txhash
		gtx.Gas = hexutil.Uint64(tx.Gas())
		gtx.GasPrice = hexutil.Big(*tx.GasPrice())
		gtx.Tx = *tx.GetZZSTX()
		return
	} else {
		e = errors.New("tx not exist")
		return
	}
}

type TxReceipt struct {
	State   uint64
	TxHash  keys.Uint256
	BNum    uint64
	BHash   keys.Uint256
	Outs    []keys.Uint256
	Nils    []keys.Uint256
	Pkgs    []keys.Uint256
	ShareId *keys.Uint256
	PoolId  *keys.Uint256
}

func (s *PublicFlightAPI) GetTxReceipt(ctx context.Context, txhash keys.Uint256) (ret *TxReceipt, e error) {
	hash := common.Hash{}
	copy(hash[:], txhash[:])

	tx, bhash, bnum, tindex := rawdb.ReadTransaction(s.exchange.b.ChainDb(), hash)
	if tx == nil {
		return
	}

	receipts, err := s.exchange.b.GetReceipts(ctx, bhash)
	if err != nil {
		e = err
		return
	}
	if len(receipts) <= int(tindex) {
		e = errors.New("the receipts count is not match")
		return
	}
	receipt := receipts[tindex]

	blocks, err := flight.SRI_Inst.GetBlocksInfoByDelay(bnum, 1, 0)
	if err != nil {
		e = err
		return
	}
	if len(blocks) != 1 {
		return
	}
	if blocks[0].Hash != *bhash.HashToUint256() {
		e = errors.New("block hash is not match")
		return
	}

	ret = &TxReceipt{}

	for _, out := range blocks[0].Outs {
		if out.State.TxHash == txhash {
			ret.Outs = append(ret.Outs, out.Root)
		}
	}

	for _, oin := range tx.GetZZSTX().Desc_O.Ins {
		ret.Nils = append(ret.Nils, oin.Root)
	}

	for _, oin := range tx.GetZZSTX().Desc_Z.Ins {
		ret.Nils = append(ret.Nils, oin.Trace)
	}

	if receipt.ShareId != nil {
		ret.ShareId = receipt.ShareId.HashToUint256()
	}
	if receipt.PoolId != nil {
		ret.PoolId = receipt.PoolId.HashToUint256()
	}

	ret.State = receipt.Status
	ret.BNum = bnum
	ret.BHash = *bhash.HashToUint256()
	ret.TxHash = txhash

	return
}
