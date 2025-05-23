// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/dex"
)

// A Trader is a client routine to interact with the server. Each Trader passed
// to runTrader will get its own *Mantle, which embed *core.Core and provides
// some additional utilities.
type Trader interface {
	// SetupWallets creates the Trader's wallets.
	SetupWallets(*Mantle)
	// HandleNotification is a receiver for core.Notifications. The Trader will
	// use the provided *Mantle to perform any requisite actions in response to
	// the Notification. Trading can begin as soon as the *BondPostNote with
	// subject AccountRegisteredSubject is received.
	HandleNotification(*Mantle, core.Notification)
	// HandleBookNote(*Mantle, *core.BookUpdate)
}

// runTrader is the LoadBot workhorse. Creates a new mantle and runs the Trader.
// runTrader will block until the ctx is canceled.
func runTrader(t Trader, name string) {
	m, err := newMantle(name)
	if err != nil {
		log.Errorf("failed to create new Mantle: %v", err)
		return
	}

	t.SetupWallets(m)
	time.Sleep(time.Second * 3)

	if ctx.Err() != nil {
		return
	}
	cert := filepath.Join(dextestDir, "dcrdex", "rpc.cert")
	exchange, err := m.GetDEXConfig(hostAddr, cert)
	if err != nil {
		m.fatalError("unable to get dex config: %v", err)
		return
	}
	bondAsset := exchange.BondAssets[unbip(regAsset)]
	if bondAsset == nil {
		m.fatalError("dex does not support asset %v for bonds", unbip(regAsset))
		return
	}
	bond := bondAsset.Amt

	err = m.Login(pass)
	if err != nil {
		m.fatalError("login error: %v", err)
		return
	}

	select {
	// wait at least a block for reg funds
	case <-time.After(time.Second * 11):
	case <-ctx.Done():
		m.fatalError("%v", ctx.Err())
		return
	}

	maintain := true
	_, err = m.PostBond(&core.PostBondForm{
		Addr:         hostAddr,
		Cert:         cert,
		AppPass:      pass,
		Bond:         bond * tradingTier,
		MaintainTier: &maintain,
		Asset:        &regAsset,
	})
	if err != nil {
		m.fatalError("registration error: %v", err)
		return
	}

	approveToken := func(assetID uint32) error {
		if asset.TokenInfo(assetID) == nil {
			return nil
		}
		symbol := dex.BipIDSymbol(assetID)
		approved := make(chan struct{})
		if _, err := m.ApproveToken(pass, assetID, hostAddr, func() {
			close(approved)
		}); err != nil && !isApprovalPendingError(err) {
			return fmt.Errorf("error approving %s token: %v", symbol, err)
		}
		m.log.Infof("Waiting for %s token approval", symbol)
		select {
		case <-approved:
			m.log.Infof("%s token approved", symbol)
		case <-time.After(time.Minute * 3):
			return fmt.Errorf("%s token not approved after 3 minutes", symbol)
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	if err := approveToken(baseID); err != nil {
		m.fatalError("error approving token: %v", err)
	}
	if err := approveToken(quoteID); err != nil {
		m.fatalError("error approving token: %v", err)
	}

	defer func() {
		for _, w := range m.wallets {
			bal, err := m.AssetBalance(w.assetID)
			if err != nil {
				log.Errorf("error updating %s balance: %v", w.symbol, err)
				return
			}
			_, err = m.Send(pass, w.assetID, bal.Available*99/100, returnAddress(w.symbol), false)
			if err != nil {
				log.Errorf("failed to send funds to alpha: %v", err)
			}
		}
	}()

	notes := m.Core.NotificationFeed().C
out:
	for {
		select {
		case note := <-notes:
			if note.Severity() >= db.ErrorLevel {
				m.fatalError("Error note received: %s", mustJSON(note))
				continue
			}
			switch n := note.(type) {
			case *core.BondPostNote:
				// Once registration is complete, register for a book feed.
				if n.Topic() == core.TopicAccountRegistered {
					// Even if we're not going to use it, we need to subscribe
					// to a book feed and keep the channel empty, so that we
					// can keep receiving book feed notifications.
					_, bookFeed, err := m.SyncBook(hostAddr, baseID, quoteID)
					if err != nil {
						m.fatalError("SyncBook error: %v", err)
						break out
					}
					go func() {
						for {
							select {
							case <-bookFeed.Next():
								// If we ever enable the  thebook feed, we
								// would pass the update to the Trader here.
								// For now, just keep the channel empty.
								m.log.Tracef("book note received")
							case <-ctx.Done():
								return
							}
						}
					}()
				}
			case *core.EpochNotification:
				m.log.Debugf("Epoch note received: %s", mustJSON(note))
			case *core.MatchNote:
				if n.Topic() == core.TopicNewMatch {
					atomic.AddUint32(&matchCounter, 1)
				}
			}

			t.HandleNotification(m, note)
		case <-ctx.Done():
			break out
		}
	}

	// Let Core shutdown and lock up.
	m.waiter.WaitForShutdown()
}

// A Mantle is a wrapper for *core.Core that adds some useful LoadBot methods
// and fields.
type Mantle struct {
	*core.Core
	waiter        *dex.StartStopWaiter
	name          string
	log           dex.Logger
	wallets       map[uint32]*botWallet
	lastReplenish time.Time
}

// newMantle is a constructor for a *Mantle. Each Mantle has its own core. The
// embedded Core is initialized, but not registered.
func newMantle(name string) (*Mantle, error) {
	coreDir := filepath.Join(botDir, "mantle_"+name)
	err := os.MkdirAll(coreDir, 0700)
	if err != nil {
		return nil, fmt.Errorf("error creating core directory: %v", err)
	}
	dbPath := filepath.Join(coreDir, "core.db")
	c, err := core.New(&core.Config{
		DBPath:           dbPath,
		Net:              dex.Simnet,
		Logger:           loggerMaker.Logger("CORE:" + name),
		NoAutoWalletLock: true,
		// UnlockCoinsOnLogin: true, // true if we are certain that two bots/Core's are not using the same external wallet
		NoAutoDBBackup: true,
	})
	if err != nil {
		return nil, fmt.Errorf("error initializing core: %w", err)
	}

	waiter := dex.NewStartStopWaiter(c)
	waiter.Start(ctx)

	_, err = c.InitializeClient(pass, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize client")
	}

	m := &Mantle{
		Core:    c,
		waiter:  waiter,
		name:    name,
		log:     loggerMaker.Logger("MANTLE:" + name),
		wallets: make(map[uint32]*botWallet),
	}

	return m, nil
}

// fatalError kills the LoadBot by cancelling the global Context.
func (m *Mantle) fatalError(s string, a ...any) {
	m.log.Criticalf(s, a...)
	if !ignoreErrors || ctx.Err() != nil {
		quit()
	}
}

// order places an order on the market.
func (m *Mantle) order(sell bool, qty, rate uint64) error {
	_, err := m.Trade(pass, coreLimitOrder(sell, qty, rate))
	if err != nil {
		switch {
		case isOverLimitError(err):
			m.log.Infof("Over-limit error. Order not placed.")
		case isApprovalPendingError(err):
			m.log.Infof("Approval-pending error. Order not placed")
		default:
			m.fatalError("Trade error (limit order, sell = %t, qty = %d, rate = %d): %v", sell, qty, rate, err)
		}
		return err
	}
	atomic.AddUint32(&orderCounter, 1)
	return nil
}

type orderReq struct {
	sell bool
	qty  uint64
	rate uint64
}

// orderMetered places a series of orders spaced out over a specific redemption.
// The orders are spaced so that the period for n orders is dur / n, not
// dur / (n - 1), meaning there is no order placed at time now + dur, the last
// order is placed at now + dur - (dur / n). This is a convenience so that the
// caller can target n orders per epoch with a dur = mkt.EpochDuration, and
// avoid the simultaneous order at the beginning of the next epoch.
func (m *Mantle) orderMetered(ords []*orderReq, dur time.Duration) {
	if len(ords) == 0 {
		return
	}
	placeOrder := func() error {
		ord := ords[0]
		ords = ords[1:]
		return m.order(ord.sell, ord.qty, ord.rate)
	}
	err := placeOrder()
	if isOverLimitError(err) || isApprovalPendingError(err) {
		return
	}
	if len(ords) == 0 {
		// There was only one to place. No need to set a ticker.
		return
	}
	go func() {
		ticker := time.NewTicker(dur / time.Duration(len(ords)))
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				err := placeOrder()
				if isOverLimitError(err) || isApprovalPendingError(err) {
					return
				}
				if len(ords) == 0 {
					// There was only one to place. No need to set a ticker.
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// marketOrder places an order on the market.
func (m *Mantle) marketOrder(sell bool, qty uint64) {
	mo := coreLimitOrder(sell, qty, 0)
	mo.IsLimit = false
	_, err := m.Trade(pass, mo)
	if err != nil {
		if isOverLimitError(err) || isApprovalPendingError(err) {
			m.log.Infof("Over-limit error. Order not placed.")
		} else {
			m.fatalError("Trade error (market order, sell = %t, qty = %d: %v", sell, qty, err)
		}
		return
	}
	atomic.AddUint32(&orderCounter, 1)
}

// book gets the book, or kills LoadBot on error.
func (m *Mantle) book() *core.OrderBook {
	book, err := m.Book(hostAddr, baseID, quoteID)
	if err != nil {
		m.fatalError("sideStacker error getting order book: %v", err)
		return &core.OrderBook{}
	}
	return book
}

// truncatedMidGap is the market mid-gap value truncated to the next lowest
// multiple of BTC rate-step.
func (m *Mantle) truncatedMidGap() uint64 {
	midGap := midGap(m.book())
	return truncate(int64(midGap), int64(rateStep))
}

// createWallet creates a new wallet/account for the asset and node. If an error
// is encountered, LoadBot will be killed.
func (m *Mantle) createWallet(symbol string, minFunds, maxFunds uint64, numCoins int) {
	// Generate a name for this wallet.
	name := randomToken()
	var rpcPort string
	switch symbol {
	case eth, usdc, polygon, usdcp:
		// Nothing to do here for internal wallets.
	case dcr:
		cmdOut := <-harnessCtl(ctx, symbol, "./alpha", "createnewaccount", name)
		if cmdOut.err != nil {
			m.fatalError("%s create account error: %v", symbol, cmdOut.err)
			return
		}
		// Even though the harnessCtl is synchronous, I've still observed some
		// issues with trying to create the wallet immediately.
		<-time.After(time.Second)
	case ltc, bch, btc, dash, dgb:
		cmdOut := <-harnessCtl(ctx, symbol, "./new-wallet", alpha, name)
		if cmdOut.err != nil {
			m.fatalError("%s create account error: %v", symbol, cmdOut.err)
			return
		}
		<-time.After(time.Second)
	case doge, firo, zec, zcl:
		// Some coins require a totally new node. Create it and monitor
		// it. Shut it down with the stop function before exiting.
		addrs, err := findOpenAddrs(2)
		if err != nil {
			m.fatalError("unable to find open ports: %v", err)
			return
		}
		addrPort := addrs[0].String()
		_, rpcPort, err = net.SplitHostPort(addrPort)
		if err != nil {
			m.fatalError("unable to split addr and port: %v", err)
			return
		}
		addrPort = addrs[1].String()
		_, networkPort, err := net.SplitHostPort(addrPort)
		if err != nil {
			m.fatalError("unable to split addr and port: %v", err)
			return
		}

		// NOTE: The exec package seems to listen for a SIGINT and call
		// cmd.Process.Kill() when it happens. Because of this it seems
		// zec will error when we run the stop-wallet script because the
		// node already shut down when killing with ctrl-c. doge however
		// does not respect the kill command and still needs the wallet
		// to be stopped here. So, it is probably fine to ignore the
		// error returned from stop-wallet.
		stopFn := func(ctx context.Context) {
			<-harnessCtl(ctx, symbol, "./stop-wallet", rpcPort, name)
		}
		if err = harnessProcessCtl(symbol, stopFn, "./start-wallet", name, rpcPort, networkPort); err != nil {
			m.fatalError("%s start-wallet error: %v", symbol, err)
			return
		}
		<-time.After(time.Second * 3)
		if symbol == zec || symbol == zcl {
			<-time.After(time.Second * 10)
		}
		// Connect the new node to the alpha node. name only used with zec and zcl.
		cmdOut := <-harnessCtl(ctx, symbol, "./connect-alpha", rpcPort, name)
		if cmdOut.err != nil {
			m.fatalError("%s create account error: %v", symbol, cmdOut.err)
			return
		}
		<-time.After(time.Second)
		if symbol == zec || symbol == zcl {
			<-time.After(time.Second * 20)
		}
	default:
		m.fatalError("createWallet: symbol %s unknown", symbol)
	}

	var walletPass []byte
	if symbol == dcr {
		walletPass = pass
	}
	if rpcPort == "" {
		rpcPort = rpcAddr(symbol)
	}
	w := newBotWallet(symbol, alpha, name, rpcPort, walletPass, minFunds, maxFunds, numCoins)
	m.wallets[w.assetID] = w

	createWallet := func(walletPW []byte, form *core.WalletForm, nCoins int) (string, error) {
		err := m.CreateWallet(pass, walletPW, form)
		if err != nil {
			return "", fmt.Errorf("Mantle %s failed to create wallet: %v", m.name, err)
		}
		walletSymbol := dex.BipIDSymbol(form.AssetID)
		m.log.Infof("created wallet %s:%s", walletSymbol, name)
		coreWallet := m.WalletState(form.AssetID)
		if coreWallet == nil {
			return "", fmt.Errorf("failed to retrieve WalletState for newly created %s wallet", walletSymbol)
		}
		addr := coreWallet.Address
		if symbol == zec {
			var ua struct {
				TAddr string `json:"transparent"`
			}
			if err := json.Unmarshal([]byte(addr[len("unified:"):]), &ua); err != nil {
				return "", fmt.Errorf("error decoding unified address: %w", err)
			}
			addr = ua.TAddr
		}
		if numCoins < 1 {
			return addr, nil
		}

		deadline := time.After(time.Second * 30)
		for {
			s := m.WalletState(form.AssetID)
			if s.Synced {
				break
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return "", context.Canceled
			case <-deadline:
				return "", fmt.Errorf("timed out waiting for wallet to sync")
			}
		}

		if nCoins != 0 {
			// Send fee funding for token assets.
			if tkn := asset.TokenInfo(form.AssetID); tkn != nil {
				if err = send(dex.BipIDSymbol(tkn.ParentID), addr, 1000e9); err != nil {
					if ignoreErrors && ctx.Err() == nil {
						m.log.Errorf("Trouble sending fee funding: %v", err)
					} else {
						return "", err
					}
				}
				for {
					time.Sleep(time.Second * 3)
					bal, err := m.AssetBalance(tkn.ParentID)
					if err != nil {
						if ignoreErrors && ctx.Err() == nil {
							m.log.Errorf("Trouble sending fee funding: %v", err)
						} else {
							return "", err
						}
					}
					if bal.Available > 0 {
						break
					}
					m.log.Infof("%s fee balance not available yet. Trying again in 3 seconds", dex.BipIDSymbol(tkn.ParentID))
				}

			}
			chunk := (maxFunds + minFunds) / 2 / uint64(nCoins)
			for i := 0; i < nCoins; {
				if err = send(walletSymbol, addr, chunk); err != nil {
					if ignoreErrors && ctx.Err() == nil {
						m.log.Errorf("Trouble sending %d %s to %s: %v\n Sleeping and trying again.", fmtAtoms(chunk, walletSymbol), walletSymbol, addr, err)
						// It often happens that the wallet is not able to
						// create enough outputs. try indefinitely
						// if we are ignoring errors.
						time.Sleep(time.Second)
						continue
					}
					return "", err
				}
				i++
			}
		}

		return addr, nil
	}

	var err error
	if w.parentForm != nil {
		// Create the parent asset
		if w.parentAddress, err = createWallet(walletPass, w.parentForm, 1); err != nil {
			m.fatalError("error creating parent asset wallet: %v", err)
			return
		}
		walletPass = nil
	}

	if w.address, err = createWallet(walletPass, w.form, numCoins); err != nil {
		m.fatalError(err.Error())
		return
	}

}

func send(symbol, addr string, val uint64) error {
	log.Tracef("Sending %s %s to %s", fmtAtoms(val, symbol), symbol, addr)
	var res *harnessResult
	switch symbol {
	case btc, dcr, ltc, dash, doge, firo, bch, dgb:
		res = <-harnessCtl(ctx, symbol, "./alpha", "sendtoaddress", addr, fmtConv(val, symbol))
	case zec, zcl:
		// sendtoaddress will choose spent outputs if a block was
		// recently mined. Use the zecSendMtx to ensure we have waited
		// a sec after mining.
		//
		// TODO: This is not great and does not allow for multiple
		// loadbots to run on zec at once. Find a better way to avoid
		// double spends. Alternatively, wait for zec to fix this and
		// remove the lock https://github.com/zcash/zcash/issues/6045
		zecSendMtx.Lock()
		res = <-harnessCtl(ctx, symbol, "./alpha", "sendtoaddress", addr, fmtConv(val, symbol))
		zecSendMtx.Unlock()
	case eth, polygon:
		// eth values are always handled as gwei, so multiply by 1e9
		// here to convert to wei.
		res = <-harnessCtl(ctx, symbol, "./sendtoaddress", addr, strconv.FormatFloat(float64(val)/1e9, 'f', 9, 64))
	case usdc, usdcp:
		res = <-harnessCtl(ctx, symbol, "./sendUSDC", addr, strconv.FormatFloat(float64(val)/1e6, 'f', 6, 64))
	default:
		return fmt.Errorf("send unknown symbol %q", symbol)
	}
	return res.err

}

type walletMinMax map[uint32]struct {
	min, max uint64
}

// replenishBalances will run replenishBalance for all wallets using the
// provided min and max funds.
func (m *Mantle) replenishBalances(wmm walletMinMax) {
	for k, v := range wmm {
		m.replenishBalance(m.wallets[k], v.min, v.max)
	}
	// TODO: Check balance in parent wallets? We send them some initial funds,
	// and maybe that's enough for our purposes, since it just covers fees.
}

// replenishBalance will bring the balance with allowable limits by requesting
// funds from or sending funds to the wallet's node.
func (m *Mantle) replenishBalance(w *botWallet, minFunds, maxFunds uint64) {
	// Get the Balance from the user in case it changed while while this note
	// was in the notification pipeline.
	bal, err := m.AssetBalance(w.assetID)
	if err != nil {
		m.fatalError("error updating %s balance: %v", w.symbol, err)
		return
	}

	m.log.Debugf("Balance note received for %s (minFunds = %s, maxFunds = %s): %s",
		w.symbol, fmtConv(minFunds, w.symbol), fmtConv(maxFunds, w.symbol), mustJSON(bal))

	// If over or under max, make the average of the two.
	wantBal := (maxFunds + minFunds) / 2

	if bal.Available < minFunds {
		chunk := (wantBal - bal.Available) / uint64(w.numCoins)
		for i := 0; i < w.numCoins; {
			m.log.Debugf("Requesting %s from %s alpha node", fmtAtoms(chunk, w.symbol), w.symbol)
			if err = send(w.symbol, w.address, chunk); err != nil {
				if ignoreErrors && ctx.Err() == nil {
					m.log.Errorf("Trouble sending %d %s to %s: %v\n Sleeping and trying again.",
						fmtAtoms(chunk, w.symbol), w.symbol, w.address, err)
					// It often happens that the wallet is not able to
					// create enough outputs. try indefinitely
					// if we are ignoring errors.
					time.Sleep(3)
					time.Sleep(time.Second)
					continue
				}
				m.fatalError("error refreshing balance for %s: %v", w.symbol, err)
				return
			}
			i++
		}
	} else if bal.Available > maxFunds {
		// Send some back to the alpha address.
		amt := bal.Available - wantBal
		m.log.Debugf("Sending %s back to %s alpha node", fmtAtoms(amt, w.symbol), w.symbol)
		_, err := m.Send(pass, w.assetID, amt, returnAddress(w.symbol), false)
		if err != nil {
			m.fatalError("failed to send funds to alpha: %v", err)
		}
		time.Sleep(time.Second * 3)
	}
}

// mustJSON JSON-encodes the thing. If an error is encountered, the error text
// is returned instead.
func mustJSON(thing any) string {
	s, err := json.Marshal(thing)
	if err != nil {
		return "invalid json: " + err.Error()
	}
	return string(s)
}

// fmtAtoms returns a string representation of the value in conventional
// units.
func fmtAtoms(v uint64, assetSymbol string) string {
	assetID, _ := dex.BipSymbolID(assetSymbol)
	ui, _ := asset.UnitInfo(assetID)
	return ui.FormatAtoms(v)
}

func fmtConv(v uint64, assetSymbol string) string {
	assetID, found := dex.BipSymbolID(assetSymbol)
	if !found {
		return "<unknown symbol>"
	}
	if ui, err := asset.UnitInfo(assetID); err == nil {
		return ui.ConventionalString(v)
	}
	return "<unknown asset>"
}

// coreOrder creates a *core.Order.
func coreLimitOrder(sell bool, qty, rate uint64) *core.TradeForm {
	return &core.TradeForm{
		Host:    hostAddr,
		IsLimit: true,
		Sell:    sell,
		Base:    baseID,
		Quote:   quoteID,
		Qty:     qty,
		Rate:    rate,
		TifNow:  false,
	}
}

// midGap parses the provided order book for the mid-gap price. If the book
// is empty, a default value is returned instead.
func midGap(book *core.OrderBook) uint64 {
	if keepMidGap {
		return uint64(defaultMidGap * rateEncFactor)
	}
	if len(book.Sells) > 0 {
		if len(book.Buys) > 0 {
			return (book.Buys[0].MsgRate + book.Sells[0].MsgRate) / 2
		}
		return book.Sells[0].MsgRate
	}
	if len(book.Buys) > 0 {
		return book.Buys[0].MsgRate
	}
	return uint64(defaultMidGap * rateEncFactor)
}

// truncate rounds the provided v down to an integer-multiple of mod.
func truncate(v, mod int64) uint64 {
	t := uint64(v - (v % mod))
	if t < uint64(mod) {
		return uint64(mod)
	}
	return t
}

const walletNameLength = 4

var chars = []byte("123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz")

func randomToken() string {
	b := make([]byte, walletNameLength)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// botWallet is the local wallet representation. Mantle uses the botWallet to
// keep the Core wallet's balance within allowable range.
type botWallet struct {
	form          *core.WalletForm
	parentForm    *core.WalletForm
	minFunds      uint64
	maxFunds      uint64
	name          string
	node          string
	symbol        string
	pass          []byte
	assetID       uint32
	address       string
	parentAddress string
	numCoins      int
}

// newBotWallet is the constructor for a botWallet. For a botWallet created
// with Mantle.createWallet, the botWallet's balance will be replenished up to
// once per epoch, if it falls outside of the range [minFunds, maxFunds].
// Set numCoins to at least twice the the maximum number of (booked + epoch)
// orders the wallet is expected to support.
func newBotWallet(symbol, node, name string, port string, pass []byte, minFunds, maxFunds uint64, numCoins int) *botWallet {
	var form, parentForm *core.WalletForm
	switch symbol {
	case dcr:
		form = &core.WalletForm{
			Type:    "dcrwalletRPC",
			AssetID: dcrID,
			Config: map[string]string{
				"account":   name,
				"username":  "user",
				"password":  "pass",
				"rpccert":   filepath.Join(dextestDir, "dcr/"+node+"/rpc.cert"),
				"rpclisten": port,
			},
		}
	case btc:
		form = &core.WalletForm{
			Type:    "bitcoindRPC",
			AssetID: btcID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case ltc:
		form = &core.WalletForm{
			Type:    "litecoindRPC",
			AssetID: ltcID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case bch:
		form = &core.WalletForm{
			Type:    "bitcoindRPC",
			AssetID: bchID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case zec:
		form = &core.WalletForm{
			Type:    "zcashdRPC",
			AssetID: zecID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case zcl:
		form = &core.WalletForm{
			Type:    "zclassicdRPC",
			AssetID: zclID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case dash:
		form = &core.WalletForm{
			Type:    "dashdRPC",
			AssetID: dashID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case doge:
		form = &core.WalletForm{
			Type:    "dogecoindRPC",
			AssetID: dogeID,
			Config: map[string]string{
				"walletname":   name,
				"rpcuser":      "user",
				"rpcpassword":  "pass",
				"rpcport":      port,
				"feeratelimit": "40000",
			},
		}
	case dgb:
		form = &core.WalletForm{
			Type:    "digibytedRPC",
			AssetID: dgbID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case firo:
		form = &core.WalletForm{
			Type:    "firodRPC",
			AssetID: firoID,
			Config: map[string]string{
				"walletname":  name,
				"rpcuser":     "user",
				"rpcpassword": "pass",
				"rpcport":     port,
			},
		}
	case eth, usdc:
		rpcProvider := "ws://127.0.0.1:38557"
		form = &core.WalletForm{
			Type:    "rpc",
			AssetID: ethID,
			Config: map[string]string{
				"providers": rpcProvider,
			},
		}
		if symbol == usdc {
			parentForm = form
			form = &core.WalletForm{
				Type:       "token",
				AssetID:    usdcID,
				ParentForm: form,
			}
		}
	case polygon, usdcp:
		rpcProvider := filepath.Join(dextestDir, "polygon", "alpha", "bor", "bor.ipc")
		if node == beta {
			rpcProvider = filepath.Join(dextestDir, "eth", "beta", "bor", "bor.ipc")
		}
		form = &core.WalletForm{
			Type:    "rpc",
			AssetID: polygonID,
			Config: map[string]string{
				"providers": rpcProvider,
			},
		}
		if symbol == usdcp {
			parentForm = form
			form = &core.WalletForm{
				Type:       "token",
				AssetID:    usdcpID,
				ParentForm: form,
			}
		}
	}
	return &botWallet{
		form:       form,
		parentForm: parentForm,
		name:       name,
		node:       node,
		symbol:     symbol,
		pass:       pass,
		assetID:    form.AssetID,
		numCoins:   numCoins,
		minFunds:   minFunds,
		maxFunds:   maxFunds,
	}
}

// isOverLimitError will be true if the error is a ErrQuantityTooHigh,
// indicating the client has reached its order limit. Ideally, Core would
// know the limit and we could query it to use in our algorithm, but the order
// limit change is new and Core doesn't know what to do with it yet.
func isOverLimitError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "order quantity exceeds user limit")
}

func isApprovalPendingError(err error) bool {
	return errors.Is(err, asset.ErrApprovalPending)
}
