/*
 * Copyright (c) 2013 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/conformal/btcjson"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwallet/tx"
	"github.com/conformal/btcwallet/wallet"
	"github.com/conformal/btcwire"
	"github.com/conformal/btcws"
	"path/filepath"
	"sync"
	"time"
)

// ErrNotFound describes an error where a map lookup failed due to a
// key not being in the map.
var ErrNotFound = errors.New("not found")

// addressAccountMap holds a map of addresses to names of the
// accounts that hold each address.
var addressAccountMap = struct {
	sync.RWMutex
	m map[string]string
}{
	m: make(map[string]string),
}

// MarkAddressForAccount marks an address as belonging to an account.
func MarkAddressForAccount(address, account string) {
	addressAccountMap.Lock()
	addressAccountMap.m[address] = account
	addressAccountMap.Unlock()
}

// LookupAccountByAddress returns the account name for address.  error
// will be set to ErrNotFound if the address has not been marked as
// associated with any account.
func LookupAccountByAddress(address string) (string, error) {
	addressAccountMap.RLock()
	defer addressAccountMap.RUnlock()
	account, ok := addressAccountMap.m[address]
	if !ok {
		return "", ErrNotFound
	}
	return account, nil
}

// Account is a structure containing all the components for a
// complete wallet.  It contains the Armory-style wallet (to store
// addresses and keys), and tx and utxo data stores, along with locks
// to prevent against incorrect multiple access.
type Account struct {
	*wallet.Wallet
	mtx                 sync.RWMutex
	name                string
	dirty               bool
	fullRescan          bool
	NewBlockTxJSONID    uint64
	SpentOutpointJSONID uint64
	UtxoStore           struct {
		sync.RWMutex
		dirty bool
		s     tx.UtxoStore
	}
	TxStore struct {
		sync.RWMutex
		dirty bool
		s     tx.TxStore
	}
}

// Lock locks the underlying wallet for an account.
func (a *Account) Lock() error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	err := a.Wallet.Lock()
	if err == nil {
		NotifyWalletLockStateChange(a.Name(), true)
	}
	return err
}

// Unlock unlocks the underlying wallet for an account.
func (a *Account) Unlock(passphrase []byte, timeout int64) error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	err := a.Wallet.Unlock(passphrase)
	if err == nil {
		NotifyWalletLockStateChange(a.Name(), false)
	}
	return a.Wallet.Unlock(passphrase)
}

// Rollback reverts each stored Account to a state before the block
// with the passed chainheight and block hash was connected to the main
// chain.  This is used to remove transactions and utxos for each wallet
// that occured on a chain no longer considered to be the main chain.
func (a *Account) Rollback(height int32, hash *btcwire.ShaHash) {
	a.UtxoStore.Lock()
	a.UtxoStore.dirty = a.UtxoStore.dirty || a.UtxoStore.s.Rollback(height, hash)
	a.UtxoStore.Unlock()

	a.TxStore.Lock()
	a.TxStore.dirty = a.TxStore.dirty || a.TxStore.s.Rollback(height, hash)
	a.TxStore.Unlock()

	if err := a.writeDirtyToDisk(); err != nil {
		log.Errorf("cannot sync dirty wallet: %v", err)
	}
}

// AddressUsed returns whether there are any recorded transactions spending to
// a given address.  Assumming correct TxStore usage, this will return true iff
// there are any transactions with outputs to this address in the blockchain or
// the btcd mempool.
func (a *Account) AddressUsed(pkHash []byte) bool {
	// This can be optimized by recording this data as it is read when
	// opening an account, and keeping it up to date each time a new
	// received tx arrives.

	a.TxStore.RLock()
	defer a.TxStore.RUnlock()

	for i := range a.TxStore.s {
		rtx, ok := a.TxStore.s[i].(*tx.RecvTx)
		if !ok {
			continue
		}

		if bytes.Equal(rtx.ReceiverHash, pkHash) {
			return true
		}
	}
	return false
}

// CalculateBalance sums the amounts of all unspent transaction
// outputs to addresses of a wallet and returns the balance as a
// float64.
//
// If confirmations is 0, all UTXOs, even those not present in a
// block (height -1), will be used to get the balance.  Otherwise,
// a UTXO must be in a block.  If confirmations is 1 or greater,
// the balance will be calculated based on how many how many blocks
// include a UTXO.
func (a *Account) CalculateBalance(confirms int) float64 {
	var bal uint64 // Measured in satoshi

	bs, err := GetCurBlock()
	if bs.Height == int32(btcutil.BlockHeightUnknown) || err != nil {
		return 0.
	}

	a.UtxoStore.RLock()
	for _, u := range a.UtxoStore.s {
		// Utxos not yet in blocks (height -1) should only be
		// added if confirmations is 0.
		if confirms == 0 || (u.Height != -1 && int(bs.Height-u.Height+1) >= confirms) {
			bal += u.Amt
		}
	}
	a.UtxoStore.RUnlock()
	return float64(bal) / float64(btcutil.SatoshiPerBitcoin)
}

// CalculateAddressBalance sums the amounts of all unspent transaction
// outputs to a single address's pubkey hash and returns the balance
// as a float64.
//
// If confirmations is 0, all UTXOs, even those not present in a
// block (height -1), will be used to get the balance.  Otherwise,
// a UTXO must be in a block.  If confirmations is 1 or greater,
// the balance will be calculated based on how many how many blocks
// include a UTXO.
func (a *Account) CalculateAddressBalance(pubkeyHash []byte, confirms int) float64 {
	var bal uint64 // Measured in satoshi

	bs, err := GetCurBlock()
	if bs.Height == int32(btcutil.BlockHeightUnknown) || err != nil {
		return 0.
	}

	a.UtxoStore.RLock()
	for _, u := range a.UtxoStore.s {
		// Utxos not yet in blocks (height -1) should only be
		// added if confirmations is 0.
		if confirms == 0 || (u.Height != -1 && int(bs.Height-u.Height+1) >= confirms) {
			if bytes.Equal(pubkeyHash, u.AddrHash[:]) {
				bal += u.Amt
			}
		}
	}
	a.UtxoStore.RUnlock()
	return float64(bal) / float64(btcutil.SatoshiPerBitcoin)
}

// CurrentAddress gets the most recently requested Bitcoin payment address
// from an account.  If the address has already been used (there is at least
// one transaction spending to it in the blockchain or btcd mempool), the next
// chained address is returned.
func (a *Account) CurrentAddress() (string, error) {
	a.mtx.RLock()
	addr, err := a.Wallet.LastChainedAddress()
	a.mtx.RUnlock()

	if err != nil {
		return "", err
	}

	// Get next chained address if the last one has already been used.
	pkHash, _, _ := btcutil.DecodeAddress(addr)
	if a.AddressUsed(pkHash) {
		addr, err = a.NewAddress()
	}

	return addr, err
}

// ListTransactions returns a slice of maps with details about a recorded
// transaction.  This is intended to be used for listtransactions RPC
// replies.
func (a *Account) ListTransactions(from, count int) ([]map[string]interface{}, error) {
	// Get current block.  The block height used for calculating
	// the number of tx confirmations.
	bs, err := GetCurBlock()
	if err != nil {
		return nil, err
	}

	var txInfoList []map[string]interface{}
	a.mtx.RLock()
	a.TxStore.RLock()

	lastLookupIdx := len(a.TxStore.s) - count
	// Search in reverse order: lookup most recently-added first.
	for i := len(a.TxStore.s) - 1; i >= from && i >= lastLookupIdx; i-- {
		switch e := a.TxStore.s[i].(type) {
		case *tx.SendTx:
			infos := e.TxInfo(a.name, bs.Height, a.Net())
			txInfoList = append(txInfoList, infos...)

		case *tx.RecvTx:
			info := e.TxInfo(a.name, bs.Height, a.Net())
			txInfoList = append(txInfoList, info)
		}
	}
	a.mtx.RUnlock()
	a.TxStore.RUnlock()

	return txInfoList, nil
}

// ListAddressTransactions returns a slice of maps with details about a
// recorded transactions to or from any address belonging to a set.  This is
// intended to be used for listaddresstransactions RPC replies.
func (a *Account) ListAddressTransactions(pkHashes map[string]struct{}) (
	[]map[string]interface{}, error) {

	// Get current block.  The block height used for calculating
	// the number of tx confirmations.
	bs, err := GetCurBlock()
	if err != nil {
		return nil, err
	}

	var txInfoList []map[string]interface{}
	a.mtx.RLock()
	a.TxStore.RLock()

	for i := range a.TxStore.s {
		rtx, ok := a.TxStore.s[i].(*tx.RecvTx)
		if !ok {
			continue
		}
		if _, ok := pkHashes[string(rtx.ReceiverHash[:])]; ok {
			info := rtx.TxInfo(a.name, bs.Height, a.Net())
			txInfoList = append(txInfoList, info)
		}
	}
	a.mtx.RUnlock()
	a.TxStore.RUnlock()

	return txInfoList, nil
}

// ListAllTransactions returns a slice of maps with details about a recorded
// transaction.  This is intended to be used for listalltransactions RPC
// replies.
func (a *Account) ListAllTransactions() ([]map[string]interface{}, error) {
	// Get current block.  The block height used for calculating
	// the number of tx confirmations.
	bs, err := GetCurBlock()
	if err != nil {
		return nil, err
	}

	var txInfoList []map[string]interface{}
	a.mtx.RLock()
	a.TxStore.RLock()

	// Search in reverse order: lookup most recently-added first.
	for i := len(a.TxStore.s) - 1; i >= 0; i-- {
		switch e := a.TxStore.s[i].(type) {
		case *tx.SendTx:
			infos := e.TxInfo(a.name, bs.Height, a.Net())
			txInfoList = append(txInfoList, infos...)

		case *tx.RecvTx:
			info := e.TxInfo(a.name, bs.Height, a.Net())
			txInfoList = append(txInfoList, info)
		}
	}
	a.mtx.RUnlock()
	a.TxStore.RUnlock()

	return txInfoList, nil
}

// DumpPrivKeys returns the WIF-encoded private keys for all addresses
// non-watching addresses in a wallets.
func (a *Account) DumpPrivKeys() ([]string, error) {
	a.mtx.RLock()
	defer a.mtx.RUnlock()

	// Iterate over each active address, appending the private
	// key to privkeys.
	var privkeys []string
	for _, addr := range a.ActiveAddresses() {
		key, err := a.AddressKey(addr.Address)
		if err != nil {
			return nil, err
		}
		encKey, err := btcutil.EncodePrivateKey(key.D.Bytes(),
			a.Net(), addr.Compressed)
		if err != nil {
			return nil, err
		}
		privkeys = append(privkeys, encKey)
	}

	return privkeys, nil
}

// DumpWIFPrivateKey returns the WIF encoded private key for a
// single wallet address.
func (a *Account) DumpWIFPrivateKey(address string) (string, error) {
	a.mtx.RLock()
	defer a.mtx.RUnlock()

	// Get private key from wallet if it exists.
	key, err := a.AddressKey(address)
	if err != nil {
		return "", err
	}

	// Get address info.  This is needed to determine whether
	// the pubkey is compressed or not.
	info, err := a.AddressInfo(address)
	if err != nil {
		return "", err
	}

	// Return WIF-encoding of the private key.
	return btcutil.EncodePrivateKey(key.D.Bytes(), a.Net(), info.Compressed)
}

// ImportPrivKey imports a WIF-encoded private key into an account's wallet.
// This function is not recommended, as it gives no hints as to when the
// address first appeared (not just in the blockchain, but since the address
// was first generated, or made public), and will cause all future rescans to
// start from the genesis block.
func (a *Account) ImportPrivKey(wif string, rescan bool) error {
	bs := &wallet.BlockStamp{}
	addr, err := a.ImportWIFPrivateKey(wif, bs)
	if err != nil {
		return err
	}

	if rescan {
		addrs := map[string]struct{}{
			addr: struct{}{},
		}

		a.RescanAddresses(bs.Height, addrs)
	}
	return nil
}

// ImportWIFPrivateKey takes a WIF-encoded private key and adds it to the
// wallet.  If the import is successful, the payment address string is
// returned.
func (a *Account) ImportWIFPrivateKey(wif string, bs *wallet.BlockStamp) (string, error) {
	// Decode WIF private key and perform sanity checking.
	privkey, net, compressed, err := btcutil.DecodePrivateKey(wif)
	if err != nil {
		return "", err
	}
	if net != a.Net() {
		return "", errors.New("wrong network")
	}

	// Attempt to import private key into wallet.
	a.mtx.Lock()
	addr, err := a.ImportPrivateKey(privkey, compressed, bs)
	if err != nil {
		a.mtx.Unlock()
		return "", err
	}

	// Immediately write dirty wallet to disk.
	//
	// TODO(jrick): change writeDirtyToDisk to not grab the writer lock.
	// Don't want to let another goroutine waiting on the mutex to grab
	// the mutex before it is written to disk.
	a.dirty = true
	a.mtx.Unlock()
	if err := a.writeDirtyToDisk(); err != nil {
		log.Errorf("cannot write dirty wallet: %v", err)
		return "", fmt.Errorf("import failed: cannot write wallet: %v", err)
	}

	// Associate the imported address with this account.
	MarkAddressForAccount(addr, a.Name())

	log.Infof("Imported payment address %v", addr)

	// Return the payment address string of the imported private key.
	return addr, nil
}

// Track requests btcd to send notifications of new transactions for
// each address stored in a wallet and sets up a new reply handler for
// these notifications.
func (a *Account) Track() {
	n := <-NewJSONID
	a.mtx.Lock()
	a.NewBlockTxJSONID = n
	a.mtx.Unlock()

	replyHandlers.Lock()
	replyHandlers.m[n] = a.newBlockTxOutHandler
	replyHandlers.Unlock()
	for _, addr := range a.ActiveAddresses() {
		a.ReqNewTxsForAddress(addr.Address)
	}

	n = <-NewJSONID
	a.mtx.Lock()
	a.SpentOutpointJSONID = n
	a.mtx.Unlock()

	replyHandlers.Lock()
	replyHandlers.m[n] = a.spentUtxoHandler
	replyHandlers.Unlock()
	a.UtxoStore.RLock()
	for _, utxo := range a.UtxoStore.s {
		a.ReqSpentUtxoNtfn(utxo)
	}
	a.UtxoStore.RUnlock()
}

// RescanActiveAddresses requests btcd to rescan the blockchain for new
// transactions to all active wallet addresses.  This is needed for
// catching btcwallet up to a long-running btcd process, as otherwise
// it would have missed notifications as blocks are attached to the
// main chain.
func (a *Account) RescanActiveAddresses() {
	a.mtx.RLock()
	defer a.mtx.RUnlock()

	// Determine the block to begin the rescan from.
	beginBlock := int32(0)
	if a.fullRescan {
		// Need to perform a complete rescan since the wallet creation
		// block.
		beginBlock = a.EarliestBlockHeight()
		log.Debugf("Rescanning account '%v' for new transactions since block height %v",
			a.name, beginBlock)
	} else {
		// The last synced block height should be used the starting
		// point for block rescanning.  Grab the block stamp here.
		bs := a.SyncedWith()

		log.Debugf("Rescanning account '%v' for new transactions after block height %v hash %v",
			a.name, bs.Height, bs.Hash)

		// If we're synced with block x, must scan the blocks x+1 to best block.
		beginBlock = bs.Height + 1
	}

	// Rescan active addresses starting at the determined block height.
	a.RescanAddresses(beginBlock, a.ActivePaymentAddresses())
}

// RescanAddresses requests btcd to rescan a set of addresses.  This
// is needed when, for example, importing private key(s), where btcwallet
// is synced with btcd for all but several address.
func (a *Account) RescanAddresses(beginBlock int32, addrs map[string]struct{}) {
	n := <-NewJSONID
	cmd, err := btcws.NewRescanCmd(fmt.Sprintf("btcwallet(%v)", n),
		beginBlock, addrs)
	if err != nil {
		log.Errorf("cannot create rescan request: %v", err)
		return
	}
	mcmd, err := cmd.MarshalJSON()
	if err != nil {
		log.Errorf("cannot create rescan request: %v", err)
		return
	}

	replyHandlers.Lock()
	replyHandlers.m[n] = func(result interface{}, e *btcjson.Error) bool {
		// Rescan is compatible with new txs from connected block
		// notifications, so use that handler.
		_ = a.newBlockTxOutHandler(result, e)

		if result != nil {
			// Notify frontends of new account balance.
			confirmed := a.CalculateBalance(1)
			unconfirmed := a.CalculateBalance(0) - confirmed
			NotifyWalletBalance(frontendNotificationMaster, a.name, confirmed)
			NotifyWalletBalanceUnconfirmed(frontendNotificationMaster, a.name, unconfirmed)

			return false
		}
		if bs, err := GetCurBlock(); err == nil {
			a.mtx.Lock()
			a.Wallet.SetSyncedWith(&bs)
			a.dirty = true
			a.mtx.Unlock()
			if err = a.writeDirtyToDisk(); err != nil {
				log.Errorf("cannot sync dirty wallet: %v",
					err)
			}
		}
		// If result is nil, the rescan has completed.  Returning
		// true removes this handler.
		return true
	}
	replyHandlers.Unlock()

	btcdMsgs <- mcmd
}

// SortedActivePaymentAddresses returns a slice of all active payment
// addresses in an account.
func (a *Account) SortedActivePaymentAddresses() []string {
	a.mtx.RLock()
	defer a.mtx.RUnlock()

	infos := a.SortedActiveAddresses()
	addrs := make([]string, len(infos))

	for i, addr := range infos {
		addrs[i] = addr.Address
	}

	return addrs
}

// ActivePaymentAddresses returns a set of all active pubkey hashes
// in an account.
func (a *Account) ActivePaymentAddresses() map[string]struct{} {
	a.mtx.RLock()
	defer a.mtx.RUnlock()

	infos := a.ActiveAddresses()
	addrs := make(map[string]struct{}, len(infos))

	for _, info := range infos {
		addrs[info.Address] = struct{}{}
	}

	return addrs
}

// NewAddress returns a new payment address for an account.
func (a *Account) NewAddress() (string, error) {
	a.mtx.Lock()

	// Get current block's height and hash.
	bs, err := GetCurBlock()
	if err != nil {
		return "", err
	}

	// Get next address from wallet.
	addr, err := a.NextChainedAddress(&bs)
	if err != nil {
		return "", err
	}

	// Write updated wallet to disk.
	a.dirty = true
	a.mtx.Unlock()
	if err = a.writeDirtyToDisk(); err != nil {
		log.Errorf("cannot sync dirty wallet: %v", err)
	}

	// Mark this new address as belonging to this account.
	MarkAddressForAccount(addr, a.Name())

	// Request updates from btcd for new transactions sent to this address.
	a.ReqNewTxsForAddress(addr)

	return addr, nil
}

// ReqNewTxsForAddress sends a message to btcd to request tx updates
// for addr for each new block that is added to the blockchain.
func (a *Account) ReqNewTxsForAddress(addr string) {
	log.Debugf("Requesting notifications of TXs sending to address %v", addr)

	a.mtx.RLock()
	n := a.NewBlockTxJSONID
	a.mtx.RUnlock()

	cmd := btcws.NewNotifyNewTXsCmd(fmt.Sprintf("btcwallet(%d)", n),
		[]string{addr})
	mcmd, err := cmd.MarshalJSON()
	if err != nil {
		log.Errorf("cannot request transaction notifications: %v", err)
	}

	btcdMsgs <- mcmd
}

// ReqSpentUtxoNtfn sends a message to btcd to request updates for when
// a stored UTXO has been spent.
func (a *Account) ReqSpentUtxoNtfn(u *tx.Utxo) {
	log.Debugf("Requesting spent UTXO notifications for Outpoint hash %s index %d",
		u.Out.Hash, u.Out.Index)

	a.mtx.RLock()
	n := a.SpentOutpointJSONID
	a.mtx.RUnlock()

	cmd := btcws.NewNotifySpentCmd(fmt.Sprintf("btcwallet(%d)", n),
		(*btcwire.OutPoint)(&u.Out))
	mcmd, err := cmd.MarshalJSON()
	if err != nil {
		log.Errorf("cannot create spent request: %v", err)
		return
	}

	btcdMsgs <- mcmd
}

// spentUtxoHandler is the handler function for btcd spent UTXO notifications
// resulting from transactions in newly-attached blocks.
func (a *Account) spentUtxoHandler(result interface{}, e *btcjson.Error) bool {
	if e != nil {
		log.Errorf("Spent UTXO Handler: Error %d received from btcd: %s",
			e.Code, e.Message)
		return false
	}
	v, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	txHashBE, ok := v["txhash"].(string)
	if !ok {
		log.Error("Spent UTXO Handler: Unspecified transaction hash.")
		return false
	}
	txHash, err := btcwire.NewShaHashFromStr(txHashBE)
	if err != nil {
		log.Errorf("Spent UTXO Handler: Bad transaction hash: %s", err)
		return false
	}
	index, ok := v["index"].(float64)
	if !ok {
		log.Error("Spent UTXO Handler: Unspecified index.")
	}

	_, _ = txHash, index

	// Never remove this handler.
	return false
}

// newBlockTxOutHandler is the handler function for btcd transaction
// notifications resulting from newly-attached blocks.
func (a *Account) newBlockTxOutHandler(result interface{}, e *btcjson.Error) bool {
	if e != nil {
		log.Errorf("Tx Handler: Error %d received from btcd: %s",
			e.Code, e.Message)
		return false
	}

	v, ok := result.(map[string]interface{})
	if !ok {
		// The first result sent from btcd is nil.  This could be used to
		// indicate that the request for notifications succeeded.
		if result != nil {
			log.Errorf("Tx Handler: Unexpected result type %T.", result)
		}
		return false
	}
	receiver, ok := v["receiver"].(string)
	if !ok {
		log.Error("Tx Handler: Unspecified receiver.")
		return false
	}
	receiverHash, _, err := btcutil.DecodeAddress(receiver)
	if err != nil {
		log.Errorf("Tx Handler: receiver address can not be decoded: %v", err)
		return false
	}
	height, ok := v["height"].(float64)
	if !ok {
		log.Error("Tx Handler: Unspecified height.")
		return false
	}
	blockHashBE, ok := v["blockhash"].(string)
	if !ok {
		log.Error("Tx Handler: Unspecified block hash.")
		return false
	}
	blockHash, err := btcwire.NewShaHashFromStr(blockHashBE)
	if err != nil {
		log.Errorf("Tx Handler: Block hash string cannot be parsed: %v", err)
		return false
	}
	fblockIndex, ok := v["blockindex"].(float64)
	if !ok {
		log.Error("Tx Handler: Unspecified block index.")
		return false
	}
	blockIndex := int32(fblockIndex)
	fblockTime, ok := v["blocktime"].(float64)
	if !ok {
		log.Error("Tx Handler: Unspecified block time.")
		return false
	}
	blockTime := int64(fblockTime)
	txhashBE, ok := v["txid"].(string)
	if !ok {
		log.Error("Tx Handler: Unspecified transaction hash.")
		return false
	}
	txID, err := btcwire.NewShaHashFromStr(txhashBE)
	if err != nil {
		log.Errorf("Tx Handler: Tx hash string cannot be parsed: %v", err)
		return false
	}
	ftxOutIndex, ok := v["txoutindex"].(float64)
	if !ok {
		log.Error("Tx Handler: Unspecified transaction output index.")
		return false
	}
	txOutIndex := uint32(ftxOutIndex)
	amt, ok := v["amount"].(float64)
	if !ok {
		log.Error("Tx Handler: Unspecified amount.")
		return false
	}
	pkscript58, ok := v["pkscript"].(string)
	if !ok {
		log.Error("Tx Handler: Unspecified pubkey script.")
		return false
	}
	pkscript := btcutil.Base58Decode(pkscript58)
	spent := false
	if tspent, ok := v["spent"].(bool); ok {
		spent = tspent
	}

	if int32(height) != -1 {
		worker := NotifyBalanceWorker{
			block: *blockHash,
			wg:    make(chan *sync.WaitGroup),
		}
		NotifyBalanceSyncerChans.add <- worker
		wg := <-worker.wg
		defer func() {
			wg.Done()
		}()
	}

	// Create RecvTx to add to tx history.
	t := &tx.RecvTx{
		TxID:         *txID,
		TxOutIdx:     txOutIndex,
		TimeReceived: time.Now().Unix(),
		BlockHeight:  int32(height),
		BlockHash:    *blockHash,
		BlockIndex:   blockIndex,
		BlockTime:    blockTime,
		Amount:       int64(amt),
		ReceiverHash: receiverHash,
	}

	// For transactions originating from this wallet, the sent tx history should
	// be recorded before the received history.  If wallet created this tx, wait
	// for the sent history to finish being recorded before continuing.
	req := SendTxHistSyncRequest{
		txid:     *txID,
		response: make(chan SendTxHistSyncResponse),
	}
	SendTxHistSyncChans.access <- req
	resp := <-req.response
	if resp.ok {
		// Wait until send history has been recorded.
		<-resp.c
		SendTxHistSyncChans.remove <- *txID
	}

	// Actually record the tx history.
	a.TxStore.Lock()
	a.TxStore.s.InsertRecvTx(t)
	a.TxStore.dirty = true
	a.TxStore.Unlock()

	// Notify frontends of tx.  If the tx is unconfirmed, it is always
	// notified and the outpoint is marked as notified.  If the outpoint
	// has already been notified and is now in a block, a txmined notifiction
	// should be sent once to let frontends that all previous send/recvs
	// for this unconfirmed tx are now confirmed.
	recvTxOP := btcwire.NewOutPoint(txID, txOutIndex)
	previouslyNotifiedReq := NotifiedRecvTxRequest{
		op:       *recvTxOP,
		response: make(chan NotifiedRecvTxResponse),
	}
	NotifiedRecvTxChans.access <- previouslyNotifiedReq
	if <-previouslyNotifiedReq.response {
		NotifyMinedTx <- t
		NotifiedRecvTxChans.remove <- *recvTxOP
	} else {
		// Notify frontends of new recv tx and mark as notified.
		NotifiedRecvTxChans.add <- *recvTxOP
		NotifyNewTxDetails(frontendNotificationMaster, a.Name(), t.TxInfo(a.Name(),
			int32(height), a.Wallet.Net()))
	}

	if !spent {
		u := &tx.Utxo{
			Amt:       uint64(amt),
			Height:    int32(height),
			Subscript: pkscript,
		}
		copy(u.Out.Hash[:], txID[:])
		u.Out.Index = uint32(txOutIndex)
		copy(u.AddrHash[:], receiverHash)
		copy(u.BlockHash[:], blockHash[:])
		a.UtxoStore.Lock()
		a.UtxoStore.s.Insert(u)
		a.UtxoStore.dirty = true
		a.UtxoStore.Unlock()

		// If this notification came from mempool, notify frontends of
		// the new unconfirmed balance immediately.  Otherwise, wait until
		// the blockconnected notifiation is processed.
		if u.Height == -1 {
			bal := a.CalculateBalance(0) - a.CalculateBalance(1)
			NotifyWalletBalanceUnconfirmed(frontendNotificationMaster,
				a.name, bal)
		}
	}

	// Never remove this handler.
	return false
}

// accountdir returns the directory containing an account's wallet, utxo,
// and tx files.
//
// This function is deprecated and should only be used when looking up
// old (before version 0.1.1) account directories so they may be updated
// to the new directory structure.
func accountdir(name string, cfg *config) string {
	var adir string
	if name == "" { // default account
		adir = "btcwallet"
	} else {
		adir = fmt.Sprintf("btcwallet-%s", name)
	}

	return filepath.Join(cfg.DataDir, adir)
}
