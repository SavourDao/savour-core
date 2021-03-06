package tron

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/SavourDao/savour-core/cache"
	"github.com/SavourDao/savour-core/config"
	"github.com/SavourDao/savour-core/rpc/common"
	wallet2 "github.com/SavourDao/savour-core/rpc/wallet"
	"github.com/SavourDao/savour-core/wallet"
	"github.com/SavourDao/savour-core/wallet/fallback"
	"github.com/SavourDao/savour-core/wallet/multiclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	pb "github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"math/big"
	"strings"
)

const TrxDecimals = 6

const (
	ChainName  = "trx"
	TronSymbol = "trx"
)
const (
	trc20TransferTopicLen        = 3
	trc20TransferTopic           = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	trc20TransferAddrLen         = 32
	trc20TransferMethodSignature = "a9059cbb"
	defaultGasLimit              = 1000000
)

type WalletAdaptor struct {
	fallback.WalletAdaptor
	clients *multiclient.MultiClient
}

func NewWalletAdaptor(conf *config.Config) (wallet.WalletAdaptor, error) {
	clients, err := newTronClients(conf)
	if err != nil {
		return nil, err
	}
	clis := make([]multiclient.Client, len(clients))
	for i, client := range clients {
		clis[i] = client
	}
	return &WalletAdaptor{
		clients: multiclient.New(clis),
	}, nil
}

func NewLocalWalletAdaptor(network config.NetWorkType) wallet.WalletAdaptor {
	return newWalletAdaptor(newLocalTronClient(network))
}

func newWalletAdaptor(client *tronClient) wallet.WalletAdaptor {
	return &WalletAdaptor{
		clients: multiclient.New([]multiclient.Client{client}),
	}
}

func (a *WalletAdaptor) getClient() *tronClient {
	return a.clients.BestClient().(*tronClient)
}

func (a *WalletAdaptor) GetBalance(req *wallet2.BalanceRequest) (*wallet2.BalanceResponse, error) {
	log.Info("GetBalance", "req", req)
	key := strings.Join([]string{req.Chain, req.Coin, req.Address}, ":")
	balanceCache := cache.GetBalanceCache()

	grpcClient := a.getClient().grpcClient

	var result *big.Int
	if req.ContractAddress != "" {
		symbol, err := grpcClient.TRC20GetSymbol(req.ContractAddress)
		if err != nil {
			return &wallet2.BalanceResponse{
				Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
			}, err
		}

		if symbol != req.Chain {
			err = fmt.Errorf("contract's symbol %v != symbol:%v", symbol, req.Coin)
			return &wallet2.BalanceResponse{
				Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
			}, err
		}
		result, err = grpcClient.TRC20ContractBalance(req.Address, req.ContractAddress)
		if err != nil {
			return &wallet2.BalanceResponse{
				Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
			}, err
		}
	} else {
		acc, err := grpcClient.GetAccount(req.Address)
		if err != nil {
			return &wallet2.BalanceResponse{
				Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
			}, err
		}

		if req.Coin == TronSymbol {
			//TRX
			result = big.NewInt(acc.Balance)
		} else {
			//TRC10
			if r, exist := acc.AssetV2[req.Coin]; !exist {
				result = big.NewInt(0)
			} else {
				result = big.NewInt(r)
			}
		}
	}
	balanceCache.Add(key, result)
	return &wallet2.BalanceResponse{
		Error:   &common.Error{Code: common.ReturnCode_SUCCESS},
		Balance: result.String(),
	}, nil
}

func (a *WalletAdaptor) GetTxByAddress(req *wallet2.TxAddressRequest) (*wallet2.TxAddressResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (a *WalletAdaptor) GetTxByHash(req *wallet2.TxHashRequest) (*wallet2.TxHashResponse, error) {
	log.Info("GetTxByHash", "req", req)
	grpcClient := a.getClient().grpcClient

	tx, err := grpcClient.GetTransactionByID(req.Hash)
	if err != nil {
		return &wallet2.TxHashResponse{
			Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
		}, err
	}

	r := tx.RawData.Contract
	if len(r) != 1 {
		err = fmt.Errorf("GetTxByHash, unsupport tx %v", req.Hash)
		return &wallet2.TxHashResponse{
			Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
		}, err
	}

	txi, err := grpcClient.GetTransactionInfoByID(req.Hash)
	if err != nil {
		return &wallet2.TxHashResponse{
			Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
		}, err
	}

	var depositList []depositInfo
	switch r[0].Type {
	case core.Transaction_Contract_TransferContract:
		depositList, err = decodeTransferContract(r[0], req.Hash)
		if err != nil {
			return &wallet2.TxHashResponse{
				Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
			}, err
		}

	case core.Transaction_Contract_TransferAssetContract:
		depositList, err = decodeTransferAssetContract(r[0], req.Hash)
		if err != nil {
			return &wallet2.TxHashResponse{
				Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
			}, err
		}

	case core.Transaction_Contract_TriggerSmartContract:
		depositList, err = decodeTriggerSmartContract(r[0], txi, req.Hash)
		if err != nil {
			return &wallet2.TxHashResponse{
				Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
			}, err
		}
	default:
		err = fmt.Errorf("QueryTransaction, unsupport contract type %v, tx hash %v ", r[0].Type, req.Hash)
		log.Info(err.Error())
		return &wallet2.TxHashResponse{
			Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
		}, err
	}

	//Note: decodeTriggerSmartContract supports multi TRC20 transfer in single hash,  but assume we will initiate single TRC20 transfer
	// in single hash, QueryAccountTransaction is supposed to query self-initiated transaction
	if len(depositList) > 1 {
		err = fmt.Errorf("QueryTransaction, more than 1 deposit list %v, tx hash %v ", len(depositList), req.Hash)
		log.Info(err.Error())
		return &wallet2.TxHashResponse{
			Error: &common.Error{Code: common.ReturnCode_ERROR, Brief: config.UnsupportedOperation, Detail: config.UnsupportedChain, CanRetry: true},
		}, err
	}
	var txStatus bool
	switch txi.Result {
	case core.TransactionInfo_SUCESS:
		txStatus = true
	case core.TransactionInfo_FAILED:
		txStatus = false
	}
	if len(depositList) == 0 {
		return &wallet2.TxHashResponse{
			Error: &common.Error{Code: common.ReturnCode_SUCCESS},
			Tx: &wallet2.TxMessage{
				Hash:   req.Hash,
				From:   depositList[0].fromAddr,
				To:     depositList[0].toAddr,
				Fee:    big.NewInt(txi.GetFee()).String(),
				Status: txStatus,
				Value:  depositList[0].amount,
				Type:   0,
				Height: string(txi.BlockNumber),
			},
		}, nil
	} else {
		return &wallet2.TxHashResponse{
			Error: &common.Error{Code: common.ReturnCode_SUCCESS},
			Tx: &wallet2.TxMessage{
				Hash:            req.Hash,
				From:            depositList[0].fromAddr,
				To:              depositList[0].toAddr,
				Fee:             big.NewInt(txi.GetFee()).String(),
				Status:          txStatus,
				Value:           depositList[0].amount,
				Type:            0,
				Height:          string(txi.BlockNumber),
				ContractAddress: depositList[0].contractAddr,
			},
		}, nil
	}
}

func (a *WalletAdaptor) GetAccount(req *wallet2.AccountRequest) (*wallet2.AccountResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (a *WalletAdaptor) GetUtxo(req *wallet2.UtxoRequest) (*wallet2.UtxoResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (a *WalletAdaptor) GetMinRent(req *wallet2.MinRentRequest) (*wallet2.MinRentResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (wa *WalletAdaptor) GetNonce(req *wallet2.NonceRequest) (*wallet2.NonceResponse, error) {
	log.Info("QueryNonce", "req", req)
	return &wallet2.NonceResponse{
		Error: &common.Error{Code: common.ReturnCode_SUCCESS},
		Nonce: "0",
	}, nil
}

func (wa WalletAdaptor) SendTx(req *wallet2.SendTxRequest) (*wallet2.SendTxResponse, error) {
	log.Info("SendTx", "req", req)
	var tx core.Transaction
	err := pb.Unmarshal([]byte(req.RawTx), &tx)
	if err != nil {
		return &wallet2.SendTxResponse{
			Error: &common.Error{Code: common.ReturnCode_ERROR},
		}, nil
	}

	rawData, err := pb.Marshal(tx.GetRawData())
	hash := getHash(rawData)

	_, err = wa.getClient().grpcClient.Broadcast(&tx)
	if err != nil {
		log.Error("broadcast tx failed", "hash", hex.EncodeToString(hash), "err", err)
		return &wallet2.SendTxResponse{
			Error: &common.Error{Code: common.ReturnCode_ERROR},
		}, err
	}
	log.Info("broadcast tx success", "hash", hex.EncodeToString(hash))
	return &wallet2.SendTxResponse{
		Error:  &common.Error{Code: common.ReturnCode_SUCCESS},
		TxHash: hex.EncodeToString(hash),
	}, nil
}

type semaphore chan struct{}

func (s semaphore) Acquire() {
	s <- struct{}{}
}

func (s semaphore) Release() {
	<-s
}

func stringToInt64(amount string) (int64, error) {
	log.Info("string to Int", "amount", amount)
	intAmount, success := big.NewInt(0).SetString(amount, 0)
	if !success {
		return 0, fmt.Errorf("fail to convert string%v to int64", amount)
	}
	return intAmount.Int64(), nil
}

func getHash(bz []byte) []byte {
	h := sha256.New()
	h.Write(bz)
	hash := h.Sum(nil)
	return hash
}

type depositInfo struct {
	tokenID      string
	fromAddr     string
	toAddr       string
	amount       string
	index        int
	contractAddr string
}

func decodeTransferContract(txContract *core.Transaction_Contract, txHash string) ([]depositInfo, error) {
	var tc core.TransferContract
	if err := ptypes.UnmarshalAny(txContract.GetParameter(), &tc); err != nil {
		return nil, err
	}
	fromAddress := address.Address(tc.OwnerAddress).String()
	toAddress := address.Address(tc.ToAddress).String()
	var tronDepositInfo depositInfo
	tronDepositInfo.tokenID = TronSymbol
	tronDepositInfo.fromAddr = fromAddress
	tronDepositInfo.toAddr = toAddress
	tronDepositInfo.amount = big.NewInt(tc.Amount).String()
	tronDepositInfo.contractAddr = ""
	return []depositInfo{tronDepositInfo}, nil
}

func decodeTransferAssetContract(txContract *core.Transaction_Contract, txHash string) ([]depositInfo, error) {
	var err error
	var tc core.TransferAssetContract
	if err := ptypes.UnmarshalAny(txContract.GetParameter(), &tc); err != nil {
		log.Error("UnmarshalAny TransferAssetContract", "hash", txHash, "err", err)
		return nil, err
	}
	fromAddress := address.Address(tc.OwnerAddress).String()
	toAddress := address.Address(tc.ToAddress).String()
	assetName := string(tc.AssetName)

	//	log.Info("decodeTransferAssetContract", "hash", txHash, "symbol", assetName, "fromAddress", fromAddress, "toAddress", toAddress, "amount", tc.Amount)
	var trc10DepositInfo depositInfo
	trc10DepositInfo.fromAddr = fromAddress
	trc10DepositInfo.toAddr = toAddress
	trc10DepositInfo.amount = big.NewInt(tc.Amount).String()
	trc10DepositInfo.contractAddr = assetName
	return []depositInfo{trc10DepositInfo}, err
}

func decodeTriggerSmartContract(txContract *core.Transaction_Contract, txi *core.TransactionInfo, txHash string) ([]depositInfo, error) {
	var tsc core.TriggerSmartContract
	if err := pb.Unmarshal(txContract.GetParameter().GetValue(), &tsc); err != nil {
		log.Error("decodeTriggerSmartContractLocal", "err", err, "hash", txHash)
		return nil, err
	}

	//decode only trc20transferMethod
	trc20TransferMethodByte, _ := hex.DecodeString(trc20TransferMethodSignature)
	if ok := bytes.HasPrefix(tsc.Data, trc20TransferMethodByte); !ok {
		return nil, nil
	}

	contractAddr := address.Address(tsc.ContractAddress).String()

	var depositList []depositInfo
	// check transfer info in log
	for i, txLog := range txi.Log {
		logAddrByte := []byte{}

		// transfer log topics must be 3
		if len(txLog.Topics) != trc20TransferTopicLen {
			log.Info("decodeTriggerSmartContract", "hash's len of topics is invalid", txHash)
			continue
		}
		if hex.EncodeToString(txLog.Topics[0]) == trc20TransferTopic {
			if len(txLog.Topics[1]) != trc20TransferAddrLen || len(txLog.Topics[2]) != trc20TransferAddrLen {
				log.Debug("decodeTriggerSmartContract", "invalid transfer addr len", txHash)
				continue
			}
			//address is 20 bytes
			fromBytes := txLog.Topics[1][12:]
			toBytes := txLog.Topics[2][12:]
			logAddrByte = append([]byte{address.TronBytePrefix}, fromBytes...)
			fromAddr := address.Address(logAddrByte).String()
			logAddrByte = append([]byte{address.TronBytePrefix}, toBytes...)
			toAddr := address.Address(logAddrByte).String()
			amount := new(big.Int).SetBytes(txLog.Data)

			//	log.Info("decodeTriggerSmartContract", "hash", txHash, "from", fromAddr, "to", toAddr, "amount", amount)

			var trc20DepositInfo depositInfo
			trc20DepositInfo.amount = amount.String()
			trc20DepositInfo.fromAddr = fromAddr
			trc20DepositInfo.toAddr = toAddr
			trc20DepositInfo.index = i
			trc20DepositInfo.contractAddr = contractAddr
			depositList = append(depositList, trc20DepositInfo)

		} else {
			//	log.Debug("decodeTriggerSmartContract", "hash is not transfer method", txHash)
			continue
		}
	}

	return depositList, nil
}

//IMPORTANT, current support only 1 TRC20 transfer
func decodeTriggerSmartContractLocal(txContract *core.Transaction_Contract, txHash string) ([]depositInfo, error) {
	var tsc core.TriggerSmartContract
	if err := pb.Unmarshal(txContract.GetParameter().GetValue(), &tsc); err != nil {
		log.Error("decodeTriggerSmartContractLocal", "err", err, "hash", txHash)
		return nil, err
	}

	//decode only trc20transferMethod
	trc20TransferMethodByte, _ := hex.DecodeString(trc20TransferMethodSignature)
	if ok := bytes.HasPrefix(tsc.Data, trc20TransferMethodByte); !ok {
		return nil, nil
	}

	fromAddr := address.Address(tsc.OwnerAddress).String()
	contractAddr := address.Address(tsc.ContractAddress).String()

	start := len(trc20TransferMethodByte)
	end := start + trc20TransferAddrLen
	start = end - address.AddressLength + 1

	addressTron := make([]byte, 0)
	addressTron = append(addressTron, address.TronBytePrefix)
	addressTron = append(addressTron, tsc.Data[start:end]...)

	toAddr := address.Address(addressTron).String()
	amount := new(big.Int).SetBytes(tsc.Data[end:])

	var trc20DepositInfo depositInfo
	trc20DepositInfo.amount = amount.String()
	trc20DepositInfo.fromAddr = fromAddr
	trc20DepositInfo.contractAddr = contractAddr
	trc20DepositInfo.toAddr = toAddr
	trc20DepositInfo.index = 0
	return []depositInfo{trc20DepositInfo}, nil
}
