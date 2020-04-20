package tx

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Onther-Tech/plasma-evm"
	"github.com/Onther-Tech/plasma-evm/accounts"
	"github.com/Onther-Tech/plasma-evm/accounts/keystore"
	"github.com/Onther-Tech/plasma-evm/common"
	"github.com/Onther-Tech/plasma-evm/core"
	"github.com/Onther-Tech/plasma-evm/core/types"
	"github.com/Onther-Tech/plasma-evm/ethclient"
	"github.com/Onther-Tech/plasma-evm/ethdb"
	"github.com/Onther-Tech/plasma-evm/log"
	"github.com/Onther-Tech/plasma-evm/params"
)

const (
	MaxNumTask    = 500
	MaxNumKnownTx = 5
	SendDelay     = 2

	// TODO: make below configurable
	Confirmation      = 32
	ConfirmationDelay = 4
)

var (
	ErrLockedAccount    = errors.New("account is locked")
	ErrUnknownAccount   = errors.New("account not found in keystore")
	ErrKnownTransaction = errors.New("known transaction")
	ErrDuplicateRaw     = errors.New("duplicate raw transaction")
	ErrNoDuplicateRaw   = errors.New("there is no duplicate raw transaction")
)

// TODO: Add JSONRPC API for TransactionManager
type TransactionManager struct {
	config *Config

	ks      *keystore.KeyStore
	backend *ethclient.Client
	db      ethdb.Database

	currentBlockNumber *big.Int // current block number of root chian network
	gasPrice           *big.Int

	addresses []common.Address // list of account address

	confirmed   map[common.Address]RawTransactions // confirmed raw transactions
	unconfirmed map[common.Address]RawTransactions // mined but not confirmed raw transactions
	pending     map[common.Address]RawTransactions // raw transactions to be sent

	nonce map[common.Address]uint64 // account nonce

	lastInspectTime time.Time

	numKnownErr map[common.Hash]uint64 // number of know transaction error

	taskCh chan *RawTransaction

	lock         sync.RWMutex
	gasPriceLock sync.Mutex
	wg           *sync.WaitGroup
	quit         chan struct{}
}

func NewTransactionManager(ks *keystore.KeyStore, backend *ethclient.Client, db ethdb.Database, config *Config) (*TransactionManager, error) {
	tm := &TransactionManager{
		config: config,

		ks:      ks,
		db:      db,
		backend: backend,

		currentBlockNumber: new(big.Int),
		gasPrice:           new(big.Int),

		confirmed:   make(map[common.Address]RawTransactions),
		unconfirmed: make(map[common.Address]RawTransactions),
		pending:     make(map[common.Address]RawTransactions),

		nonce: make(map[common.Address]uint64),

		numKnownErr: make(map[common.Hash]uint64),

		taskCh: make(chan *RawTransaction, MaxNumTask),

		quit: make(chan struct{}),
	}

	gasPrice := ReadGasPrice(db)

	if config.MinGasPrice.Cmp(config.MaxGasPrice) > 0 {
		return nil, errors.New("min gas price cannot exceed max gas price")
	}

	if config.GasPrice.Cmp(big.NewInt(0)) == 0 {
		gasPrice = new(big.Int).Set(DefaultConfig.GasPrice)
		log.Info("Use default gas price", "gasprice", gasPrice)
	}

	if gasPrice.Cmp(config.MinGasPrice) < 0 {
		gasPrice = new(big.Int).Set(config.MinGasPrice)
		log.Warn("Gas price is below the min gas price.")
	}

	if gasPrice.Cmp(config.MaxGasPrice) > 0 {
		gasPrice = new(big.Int).Set(config.MaxGasPrice)
		log.Warn("Gas price is above the max gas price.")
	}

	tm.gasPrice = gasPrice

	numAddrs := ReadNumAddr(db)

	if numAddrs == MaxUint64 {
		return nil, errors.New("failed to read account number in database")
	}

	var (
		i   uint64
		err error
	)

	for i = 0; i < numAddrs; i++ {
		addr := ReadAddr(db, uint64(i))
		tm.addresses = append(tm.addresses, addr)

		if _, ok := tm.nonce[addr]; ok {
			log.Error("Duplicated account found", "addr", addr)
			continue
		}

		numConfirmedRawTxs := ReadNumConfirmedRawTxs(tm.db, addr)
		if numConfirmedRawTxs == MaxUint64 {
			return nil, errors.New(fmt.Sprintf("failed to read number of confirmed raw transaction of %s", addr.String()))
		}

		log.Info("Previous account loaded", "addr", addr, "numConfirmedRawTxs", numConfirmedRawTxs)

		tm.confirmed[addr] = make(RawTransactions, 0, numConfirmedRawTxs)
		var j uint64
		for ; j < numConfirmedRawTxs; j++ {
			tm.confirmed[addr] = append(tm.confirmed[addr], ReadConfirmedTx(tm.db, addr, j))
		}

		tm.unconfirmed[addr] = ReadUnconfirmedTxs(tm.db, addr)
		tm.pending[addr] = ReadPendingTxs(tm.db, addr)

		tm.nonce[addr] = ReadAddrNonce(db, addr)
		if tm.nonce[addr] == 0 {
			tm.nonce[addr], err = backend.NonceAt(context.Background(), addr, nil)
			if err != nil {
				log.Error("Failed to read account nonce", "err", err)
				return nil, err
			}
			WriteAddrNonce(db, addr, tm.nonce[addr])
		}
		log.Info("Previous transactions are loaded", "addr", addr, "txs", len(tm.pending[addr]))
		tm.inspect(addr)
	}

	log.Info("Transaction manager loaded", "numAccounts", numAddrs)

	return tm, nil
}

// Add adds raw transaction to confirmed.
func (tm *TransactionManager) Add(account accounts.Account, raw *RawTransaction, duplicate bool) error {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	addr := account.Address

	tm.inspect(addr)

	if !tm.ks.HasAddress(addr) {
		return ErrUnknownAccount
	}

	// Update database for the first raw transaction from the account.
	if tm.indexOf(addr) < 0 {

		n := len(tm.addresses)
		WriteNumAddr(tm.db, uint64(n+1))

		tm.addresses = append(tm.addresses, addr)
		WriteAddr(tm.db, uint64(n), addr)

		tm.confirmed[addr] = RawTransactions{}
		tm.unconfirmed[addr] = RawTransactions{}
		tm.pending[addr] = RawTransactions{}

		log.Debug("New account is added to transaction manager", "addr", addr)
	}

	if !duplicate {
		// add unique aw transaction

		if previous := ReadRawTxHash(tm.db, addr, raw.Hash()); previous != nil {
			return ErrDuplicateRaw
		}
		WriteRawTxHash(tm.db, addr, *raw)
	} else {
		// add duplicate raw transaction

		if previous := ReadRawTxHash(tm.db, addr, raw.Hash()); previous == nil {
			return ErrNoDuplicateRaw
		}
	}

	// assign index
	i := ReadNumRawTxs(tm.db, addr)
	raw.Index = i
	WriteNumRawTxs(tm.db, addr, i+1)

	// assign nonce
	raw.Nonce = big.NewInt(int64(tm.nonce[addr]))
	tm.nonce[addr]++
	WriteAddrNonce(tm.db, addr, tm.nonce[addr])

	// enqueue raw transaction
	tm.pending[addr] = append(tm.pending[addr], raw)
	WritePendingTxs(tm.db, tm.quit, addr, tm.pending[addr])

	log.Info("Raw transaction added", "caption", raw.getCaption(), "from", raw.From)

	return nil
}

// TODO: rename to Has
// Count returns the number of raw transactions corresponding to the transaction.
func (tm *TransactionManager) Count(account accounts.Account, tx *types.Transaction) uint64 {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	if tm.confirmed[account.Address] == nil || len(tm.confirmed[account.Address]) == 0 {
		return 0
	}

	var count uint64
	hash := tx.Hash()

	for _, queuedTx := range tm.confirmed[account.Address] {
		if queuedTx.Hash() == hash {
			count++
		}
	}

	return count
}

func (tm *TransactionManager) Start() {
	tm.wg = new(sync.WaitGroup)
	go tm.confirmLoop()

	// send a single raw transaction to root chain.
	// TODO: make it safe under root chain provider disconnect
	send := func(addr common.Address, raw *RawTransaction) (common.Hash, error) {
		raw.sendLock.Lock()
		defer raw.sendLock.Unlock()

		// short circuit if transaction was already mined
		if raw.Mined(tm.backend) {
			return raw.MinedTxHash, nil
		}

		// subscribe new block mined event
		newHeaderEvents := make(chan *types.Header)
		newHeaderSub, err := tm.backend.SubscribeNewHead(context.Background(), newHeaderEvents)

		close := func() {
			defer func() {
				if err := recover(); err != nil {
					log.Error("New block event unsubscription", "err", err)
				}
			}()
			newHeaderSub.Unsubscribe()
		}

		defer close()

		if err != nil {
			log.Error("Failed to subscribe new block event", "err", err)
		}

		clearHeaderEvent := func() {
			for len(newHeaderEvents) > 0 {
				<-newHeaderEvents
			}
		}

		// account to send transaction
		from := accounts.Account{Address: addr}

		// helper to avoid recursive read lock
		var f func() (common.Hash, error)

		f = func() (common.Hash, error) {
			blockNumber := tm.currentBlockNumber.Uint64()

			// short circuit
			if raw.LastSentBlockNumber != 0 && raw.LastSentBlockNumber+SendDelay <= blockNumber {
				log.Debug("Too early to send transaction", "delay", SendDelay)
				raw.LastSentBlockNumber = tm.currentBlockNumber.Uint64()
				return common.Hash{}, nil
			}

			tm.gasPriceLock.Lock()
			tx := raw.ToTransaction(tm.gasPrice)
			tm.gasPriceLock.Unlock()

			signedTx, err := tm.ks.SignTx(from, tx, tm.config.ChainId)

			if err != nil {
				log.Error("failed to sign transaction", "err", err, "raw", raw.Hash(), "caption", raw.getCaption(), "tx", tx.Hash())
				return signedTx.Hash(), err
			}

			// short circuit raw transaction already has same transaction.
			if raw.HasPending(signedTx) {
				return signedTx.Hash(), nil
			}

			err = raw.AddPending(signedTx)
			if err != nil {
				log.Error(err.Error(), "raw", raw.Hash(), "caption", raw.getCaption(), "tx", tx.Hash())
				return signedTx.Hash(), err
			}
			raw.LastSentBlockNumber = blockNumber

			tm.lock.Lock()
			WritePendingTxs(tm.db, tm.quit, addr, tm.pending[addr])
			tm.lock.Unlock()

			err = tm.backend.SendTransaction(context.Background(), signedTx)

			if err == nil {
				log.Info("Transaction sent", "hash", signedTx.Hash(), "nonce", raw.Nonce, "caption", raw.getCaption(), "gasprice", signedTx.GasPrice())
				return signedTx.Hash(), nil
			}

			errMessage := strings.ToLower(err.Error())

			// short circuit if operator has not enough ether
			if strings.Contains(errMessage, "insufficient funds for gas * price + value") {
				return signedTx.Hash(), core.ErrInsufficientFunds
			}

			if strings.Contains(errMessage, "replacement transaction underpriced") {
				return signedTx.Hash(), core.ErrReplaceUnderpriced
			}

			if strings.Contains(errMessage, "transaction underpriced") {
				return signedTx.Hash(), core.ErrReplaceUnderpriced
			}

			// resubmit transaction at most MAX_NUM_KNOWN_TX times.
			if strings.Contains(errMessage, "known transaction") {
				tm.numKnownErr[signedTx.Hash()]++

				if tm.numKnownErr[signedTx.Hash()] == MaxNumKnownTx {
					tm.numKnownErr[signedTx.Hash()] = 0
					return signedTx.Hash(), ErrKnownTransaction
				}

				clearHeaderEvent()

				select {
				case <-newHeaderEvents:
					return signedTx.Hash(), ErrKnownTransaction
				case <-tm.quit:
					return signedTx.Hash(), nil
				}

			}

			// resubmit transaction with nonce increased.
			if strings.Contains(errMessage, "nonce too low") || strings.Contains(errMessage, "nonce is too low") {
				// increase nonce immediately
				previousNonce := raw.Nonce.Uint64()

				tm.nonce[addr], err = tm.backend.NonceAt(context.Background(), addr, nil)

				if err != nil {
					log.Error("Failed to read account nonce", "err", err)
				} else {
					log.Warn("Account nonce has increased by another transaction", "previousNonce", previousNonce, "currentNonce", tm.nonce[addr])
					raw.Nonce = big.NewInt(int64(tm.nonce[addr]))
					WriteAddrNonce(tm.db, addr, tm.nonce[addr])
				}
				return f()

				// if more than 1 transactions are pending, increase nonce carefully.
				// TODO: count and increase nonce
				return signedTx.Hash(), nil
			}

			// return unknown error
			log.Error("Failed to send transaction to root chain.", "err", err)
			return signedTx.Hash(), err
		}

		return f()
	}

	go func() {
		ticker := time.NewTicker(tm.config.Interval)
		defer ticker.Stop()

		for {
			select {
			case _, ok := <-ticker.C:
				if !ok {
					continue
				}

				for addr, _ := range tm.pending {
					tm.wg.Add(1)
					go func(addr common.Address) {
						defer tm.wg.Done()
						log.Trace("TransactionManager iterates", "addr", addr)
						queue := tm.pending[addr]

						tm.clearQueue(addr)
						tm.confirmQueue(addr)

						if len(queue) == 0 {
							return
						}

						var raw *RawTransaction

						// find next pending raw transaction
						for _, pending := range queue {
							if !pending.Mined(tm.backend) {
								raw = pending
								break
							}
						}

						// short circuit if no pending raw transaction exists
						if raw == nil {
							return
						}

						hash, err := send(addr, raw)

						// resubmit transaction in pending intarval loop
						if err == core.ErrReplaceUnderpriced {
							log.Debug("Gas price is fixed for underpriced transaction error")
							tm.adjustGasPrice(raw, false)
							hash, err = send(addr, raw)
							return
						}

						// short circuit if operator has not enough fund.
						if err == core.ErrInsufficientFunds || err == core.ErrReplaceUnderpriced {
							log.Error("Account doesn't have enough fund to run the chain.", "addr", addr)
							hash, err = send(addr, raw)
							return
						}

						receipt, err2 := tm.backend.TransactionReceipt(context.Background(), hash)

						// short circuit if tx is already mined
						if receipt != nil {
							log.Debug("Raw transaction is already mined", "caption", raw.getCaption(), "hash", receipt.TxHash.String())
							return
						}

						if receipt == nil && err == ErrKnownTransaction && tm.numKnownErr[hash] <= MaxNumKnownTx {
							tm.numKnownErr[hash]++
							return
						}

						fixed := false

						// handle previous submit errors
						if err == ErrKnownTransaction {
							log.Debug("Gas price is fixed for known transaction error")
							fixed = true
						}

						if err2 == ethereum.NotFound {
							log.Warn("Ethereum Transaction not found. It may be pending", "err", err2, "caption", raw.getCaption(), "hash", hash.Hex())
							fixed = true
							tm.adjustGasPrice(raw, false)
						}

						if err != nil && !fixed {
							log.Debug("Unknown transaction error", "err", err)
							tm.adjustGasPrice(raw, false)
						}

						hash, err = send(addr, raw)

						if err != nil && err != ErrKnownTransaction {
							log.Error("Failed to submit block to root chain.", "err", err)
						}
					}(addr)
				}

			case <-tm.quit:
				log.Info("TransactionManager stopped")
				return
			}
		}
	}()
}

// adjustGasPrice adjust gas prices at a reasonable price.
func (tm *TransactionManager) adjustGasPrice(raw *RawTransaction, decrease bool) {
	tm.gasPriceLock.Lock()
	defer tm.gasPriceLock.Unlock()

	var previousGasPrice *big.Int

	if (raw.MinedTxHash != common.Hash{}) {
		tx, isPending, err := tm.backend.TransactionByHash(context.Background(), raw.MinedTxHash)
		if isPending || err != nil {
			return
		}
		previousGasPrice = tx.GasPrice()
	} else if len(raw.PendingTxs) == 0 {
		previousGasPrice = new(big.Int).Set(tm.gasPrice)
	} else {
		lastPendingTx := raw.PendingTxs[len(raw.PendingTxs)-1]
		previousGasPrice = new(big.Int).Set(lastPendingTx.GasPrice())
	}

	gasPrice := new(big.Int).Set(previousGasPrice)

	if decrease {
		// new gas price = previous gas price * 0.4
		gasPrice = new(big.Int).Mul(new(big.Int).Div(tm.gasPrice, big.NewInt(10)), big.NewInt(4))
	} else {
		// new gas price = previous gas price * 1.2
		gasPrice = new(big.Int).Mul(new(big.Int).Div(tm.gasPrice, big.NewInt(10)), big.NewInt(12))
	}

	// make target gas price in range
	if gasPrice.Cmp(tm.config.MinGasPrice) < 0 {
		gasPrice.Set(tm.config.MinGasPrice)
	}
	if gasPrice.Cmp(tm.config.MaxGasPrice) > 0 {
		gasPrice.Set(tm.config.MaxGasPrice)
	}

	tm.gasPrice = new(big.Int).Set(gasPrice)

	WriteGasPrice(tm.db, gasPrice)

	previousGwei := gasPriceToString(previousGasPrice)
	adjustGwei := gasPriceToString(tm.gasPrice)

	log.Info("Gas price adjusted", "caption", raw.getCaption(), "decrease", decrease,
		"previous", previousGwei,
		"adjusted", adjustGwei)
}

// clearQueue check raw transaction is mined. Mined raw transactions move to unconfirmed pending.
// Before confirmed, if the mined raw transaction is removed from root chian network, it goes back to the pending again.
func (tm *TransactionManager) clearQueue(addr common.Address) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	// short circuit if pending is nil or empty.
	if tm.pending[addr] == nil || len(tm.pending[addr]) == 0 {
		return
	}

	// check pending transaction is mined or not
	for _, raw := range tm.pending[addr] {
		ok, err := raw.CheckMined(tm.backend, false)
		if err != nil {
			log.Error("Failed to clear pending transactions. Check rootchain provider", "err", err, "caption", raw.getCaption())
			break
		}

		if !ok {
			break
		}

		log.Info("Transaction is mined", "nonce", raw.Nonce, "caption", raw.getCaption(), "reverted", raw.Reverted, "from", addr, "hash", raw.MinedTxHash.String())

		if raw.Reverted {
			log.Error("Transaction is reverted", "caption", raw.getCaption(), "hash", raw.MinedTxHash.String())
		}
		tm.adjustGasPrice(raw, true)
	}

	// remove mined raw transactions
	var minedRaws RawTransactions
	i := 0
	for ; i < len(tm.pending[addr]); i++ {
		raw := tm.pending[addr][i]
		if raw == nil {
			break
		}

		if !raw.Mined(tm.backend) {
			if raw.Reverted && !raw.AllowRevert {
				log.Error("Transaction reverted", "caption", raw.getCaption(), "hash", raw.Hash())
			}
			break
		}
		minedRaws = append(minedRaws, raw)
	}

	// update database
	l := len(minedRaws)
	if l != 0 {
		tm.unconfirmed[addr] = append(tm.unconfirmed[addr], minedRaws...)
		tm.pending[addr] = tm.pending[addr][l:]
		WritePendingTxs(tm.db, tm.quit, addr, tm.pending[addr])
		WriteUnconfirmedTxs(tm.db, tm.quit, addr, tm.unconfirmed[addr])
	}
}

// confirmQueue check mined raw transaction is confirmed.
// If unconfirmed transaction is removed from canonical chain, insert it into pending pending.
func (tm *TransactionManager) confirmQueue(addr common.Address) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	if time.Since(tm.lastInspectTime) > time.Second*5 {
		tm.inspect(addr)
	}

	// short circuit if unconfirmed is nil or empty.
	if tm.unconfirmed[addr] == nil || len(tm.unconfirmed[addr]) == 0 {
		return
	}

	// re-add removed raw transaction into queue
	var lastRemovedRaw *RawTransaction
	var newUnconfirmed RawTransactions
	for _, raw := range tm.unconfirmed[addr] {
		removed, err := raw.Removed(tm.backend)
		if err != nil {
			log.Error("Failed to check transaction is removed", "err", err)
			break
		}

		if removed {
			log.Info("Raw transaction is removed", "addr", addr, "caption", raw.getCaption())
			raw.PrepareToResend()
			tm.pending[addr] = append(tm.pending[addr], raw)
		} else {
			newUnconfirmed = append(newUnconfirmed, raw)
		}

		lastRemovedRaw = raw
	}

	// update database
	if lastRemovedRaw != nil {
		tm.unconfirmed[addr] = newUnconfirmed
		sort.Sort(RawTransactionsByIndex(tm.pending[addr]))

		WriteUnconfirmedTxs(tm.db, tm.quit, addr, tm.unconfirmed[addr])
		WritePendingTxs(tm.db, tm.quit, addr, tm.pending[addr])
	}

	// remove already confirmed raw transactions
	numConfirmed := ReadNumConfirmedRawTxs(tm.db, addr)
	currentBlockNumber := new(big.Int).Set(tm.currentBlockNumber)
	i := 0
	for ; i < len(tm.unconfirmed[addr]); i++ {
		raw := tm.unconfirmed[addr][i]
		log.Debug("check raw is confirmed", "addr", addr, "caption", raw.getCaption())

		if !raw.Confirmed(tm.backend, currentBlockNumber) {
			break
		}

		log.Info("Transaction is confirmed", "addr", addr, "caption", raw.getCaption())
		raw.ConfirmedIndex = numConfirmed
		tm.confirmed[addr] = append(tm.confirmed[addr], raw)
		WriteConfirmedTx(tm.db, addr, numConfirmed, raw)
		numConfirmed++
	}

	// update database
	if i != 0 {
		tm.unconfirmed[addr] = tm.unconfirmed[addr][i:]
		WriteNumConfirmedRawTxs(tm.db, tm.quit, addr, numConfirmed)
		WriteUnconfirmedTxs(tm.db, tm.quit, addr, tm.unconfirmed[addr])
	}
}

func (tm *TransactionManager) indexOf(addr common.Address) int {
	var i int
	for i = 0; i < len(tm.addresses); i++ {
		if tm.addresses[i] == addr {
			return i
		}
	}

	return -1
}

// TODO: use SubscribeNewHead with disconnection handling
func (tm *TransactionManager) confirmLoop() {
	tm.wg.Add(1)
	tm.wg.Done()
	closed := false

	newHeaderCh := make(chan *types.Header)
	sub, err := tm.backend.SubscribeNewHead(context.Background(), newHeaderCh)

	close := func() {
		defer func() {
			if err := recover(); err != nil {
				log.Error("New block event unsubscription", "err", err)
			}
		}()
		sub.Unsubscribe()
	}

	defer close()

	if err != nil {
		log.Error("Failed to subscribe root chian new block event", "err", err)
		return
	}

	reconnTimer := time.NewTimer(0)
	<-reconnTimer.C

	reconn := func() {
		if closed {
			return
		}

		sub2, err := tm.backend.SubscribeNewHead(context.Background(), newHeaderCh)

		if err != nil {
			log.Error("Failed to re-subscribe root chian new block event", "err", err)
			reconnTimer.Reset(5 * time.Second)
		} else {
			sub = sub2
			log.Info("Re-subscribe root chian new block event", "err", err)
		}
	}

	var lastConfirmed uint64

	for {
		select {
		case <-tm.quit:
			closed = true
			reconnTimer.Stop()
			return

		case header, ok := <-newHeaderCh:
			if !ok {
				continue
			}

			if lastConfirmed == 0 {
				lastConfirmed = header.Number.Uint64()
			}

			block, err := tm.backend.BlockByHash(context.Background(), header.Hash())
			if err != nil {
				log.Error("Failed to read root chain block", "err", err)
				continue
			}

			log.Info("New root chain block mined", "number", header.Number, "numTxs", len(block.Transactions()), "gasUsed", header.GasUsed, "gasLimit", header.GasLimit)

			tm.lock.Lock()
			tm.currentBlockNumber.Set(header.Number)
			tm.lock.Unlock()

			if lastConfirmed+ConfirmationDelay < header.Number.Uint64() {
				continue
			}

			lastConfirmed = header.Number.Uint64()
			for _, addr := range tm.addresses {
				tm.confirmQueue(addr)
			}

		case err := <-sub.Err():
			log.Error("New block event unsubscribed", "err", err)
			reconn()

		case _, ok := <-reconnTimer.C:
			if ok {
				reconn()
			}
		}
	}
}

func (tm *TransactionManager) Stop() {
	tm.wg.Wait()
	close(tm.quit)
}

func (tm *TransactionManager) inspect(addr common.Address) {
	confirmed := len(tm.confirmed[addr])
	unconfirmed := len(tm.unconfirmed[addr])
	pending := len(tm.pending[addr])
	log.Debug("Inspect queue", "addr", addr, "total", confirmed+unconfirmed+pending, "confirmed", confirmed, "unconfiemd", unconfirmed, "pending", pending)

	tm.lastInspectTime = time.Now()
}

func gasPriceToString(gp *big.Int) string {
	ngp := new(big.Float).Quo(new(big.Float).SetInt(gp), new(big.Float).SetInt64(params.GWei))
	ngp.SetPrec(10)

	return ngp.String() + " Gwei"
}
