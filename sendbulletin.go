package main

import (
	"errors"
	"sort"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/soapboxsys/ombudslib/protocol/ombproto"
	"github.com/soapboxsys/ombudslib/rpcexten"
	"github.com/soapboxsys/ombwallet/chain"
	"github.com/soapboxsys/ombwallet/txstore"
)

// TODO NOTICE
// Handles a sendbulletin json request. Attempts to send a bulletin from the
// specified address. If the address does not have enough funds or there is some
// other problem then request throws a resonable error.
func SendBulletin(w *Wallet, chainSrv *chain.Client, icmd btcjson.Cmd) (interface{}, error) {
	cmd := icmd.(rpcexten.SendBulletinCmd)

	log.Trace("Starting Send")
	// NOTE Rapid requests will serially block due to locking
	heldUnlock, err := w.HoldUnlock()
	if err != nil {
		return nil, err
	}
	defer heldUnlock.Release()
	log.Trace("Grabbed wallet lock")

	addr, err := btcutil.DecodeAddress(cmd.Address, activeNet.Params)
	if err != nil {
		return nil, err
	}
	// NOTE checks to see if addr is in the wallet
	_, err = w.Manager.Address(addr)
	if err != nil {
		log.Trace("The address is not in the manager")
		return nil, err
	}

	bs, err := chainSrv.BlockStamp()
	if err != nil {
		return nil, err
	}

	log.Trace("Looking into elgible outputs")
	// NOTE minconf is set to 1
	var eligible []txstore.Credit
	eligible, err = w.findEligibleOutputs(1, bs)
	if err != nil {
		return nil, err
	}

	msgtx := wire.NewMsgTx()

	// Create the bulletin and add bulletin TxOuts to msgtx
	bltn, err := ombproto.NewBulletinFromStr(cmd.Address, cmd.Board, cmd.Message)
	if err != nil {
		return nil, err
	}
	txouts, err := bltn.TxOuts(rpcexten.DustAmnt(), activeNet.Params)
	if err != nil {
		return nil, err
	}
	// The amount of bitcoin burned by sending the bulletin
	var totalBurn btcutil.Amount
	for _, txout := range txouts {
		msgtx.AddTxOut(txout)
		totalBurn += btcutil.Amount(txout.Value)
	}

	log.Trace("Searching for a UTXO with target address.")
	// Find the index of the credit with the target address and use that as the
	// first txin in the bulletin.
	i, err := findAddrCredit(eligible, addr)
	if err != nil {
		log.Trace("No eligible credits found for addr: %s", addr)
		return nil, err
	}

	authc := eligible[i]
	// Add authoring txin
	msgtx.AddTxIn(wire.NewTxIn(authc.OutPoint(), nil))

	// Remove the author credit
	eligible = append(eligible[:i], eligible[i+1:]...)
	sort.Sort(sort.Reverse(ByAmount(eligible)))
	totalAdded := authc.Amount()
	inputs := []txstore.Credit{authc}
	var input txstore.Credit

	for totalAdded < totalBurn {
		if len(eligible) == 0 {
			return nil, InsufficientFundsError{totalAdded, totalBurn, 0}
		}
		input, eligible = eligible[0], eligible[1:]
		inputs = append(inputs, input)
		msgtx.AddTxIn(wire.NewTxIn(input.OutPoint(), nil))
		totalAdded += input.Amount()
	}

	log.Trace("Estimating fee")
	// Initial fee estimate
	szEst := estimateTxSize(len(inputs), len(msgtx.TxOut))
	feeEst := minimumFee(w.FeeIncrement, szEst, msgtx.TxOut, inputs, bs.Height)

	// Ensure that we cover the fee and the total burn and if not add another
	// input.
	for totalAdded < totalBurn+feeEst {
		if len(eligible) == 0 {
			return nil, InsufficientFundsError{totalAdded, totalBurn, feeEst}
		}
		input, eligible = eligible[0], eligible[1:]
		inputs = append(inputs, input)
		msgtx.AddTxIn(wire.NewTxIn(input.OutPoint(), nil))
		szEst += txInEstimate
		totalAdded += input.Amount()
		feeEst = minimumFee(w.FeeIncrement, szEst, msgtx.TxOut, inputs, bs.Height)
	}

	// Shameless copy from createtx
	// changeIdx is -1 unless there's a change output.
	changeIdx := -1

	log.Trace("Formulating the transaction and computing fees")
	for {
		change := totalAdded - totalBurn - feeEst
		if change > 0 {
			// Send the change back to the authoring addr.
			pkScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return nil, err
			}
			msgtx.AddTxOut(wire.NewTxOut(int64(change), pkScript))

			changeIdx = len(msgtx.TxOut) - 1
			if err != nil {
				return nil, err
			}
		}

		log.Trace("Signing the transaction")
		if err = signMsgTx(msgtx, inputs, w.Manager); err != nil {
			return nil, err
		}

		if feeForSize(w.FeeIncrement, msgtx.SerializeSize()) <= feeEst {
			// The required fee for this size is less than or equal to what
			// we guessed, so we're done.
			break
		}

		if change > 0 {
			// Remove the change output since the next iteration will add
			// it again (with a new amount) if necessary.
			tmp := msgtx.TxOut[:changeIdx]
			tmp = append(tmp, msgtx.TxOut[changeIdx+1:]...)
			msgtx.TxOut = tmp
		}

		feeEst += w.FeeIncrement
		for totalAdded < totalBurn+feeEst {
			if len(eligible) == 0 {
				return nil, InsufficientFundsError{totalAdded, totalBurn, feeEst}
			}
			input, eligible = eligible[0], eligible[1:]
			inputs = append(inputs, input)
			msgtx.AddTxIn(wire.NewTxIn(input.OutPoint(), nil))
			szEst += txInEstimate
			totalAdded += input.Amount()
			feeEst = minimumFee(w.FeeIncrement, szEst, msgtx.TxOut, inputs, bs.Height)
		}
	}

	if err := validateMsgTx(msgtx, inputs); err != nil {
		return nil, err
	}

	log.Trace("Inserting new tx into the TxStore.")
	// Handle updating the TxStore
	if err = insertIntoStore(w.TxStore, msgtx); err != nil {
		return nil, err
	}

	txSha, err := chainSrv.SendRawTransaction(msgtx, false)
	if err != nil {
		return nil, err
	}
	log.Infof("Successfully sent bulletin %v", txSha)

	return txSha.String(), nil
}

// TODO NOTICE
var ErrNoUnspentForAddr error = errors.New("No unspent outputs for this address")

// TODO NOTICE finds a credit that is a P2PKH to the target address
func findAddrCredit(credits []txstore.Credit, target btcutil.Address) (int, error) {

	var idx int = -1
	for i, credit := range credits {
		class, addrs, _, err := credit.Addresses(activeNet.Params)
		if err != nil {
			return -1, err
		}
		switch class {
		case txscript.PubKeyHashTy:
			if target.EncodeAddress() == addrs[0].EncodeAddress() {
				idx = i
				break
			}

		// Ignore all non P2PKH txouts
		default:
			continue
		}

	}
	if idx == -1 {
		return -1, ErrNoUnspentForAddr
	}

	return idx, nil
}

// Inserts a new transaction into the TxStore, updating credits and debits
// of the store.
func insertIntoStore(store *txstore.Store, tx *wire.MsgTx) error {
	// Add to the transaction store.
	txr, err := store.InsertTx(btcutil.NewTx(tx), nil)
	if err != nil {
		log.Errorf("Error adding sent tx history: %v", err)
		return btcjson.ErrInternal
	}
	_, err = txr.AddDebits()
	if err != nil {
		log.Errorf("Error adding sent tx history: %v", err)
		return btcjson.ErrInternal
	}
	store.MarkDirty()
	return nil
}
