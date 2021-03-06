// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package swap

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/dex/wait"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/auth"
	"decred.org/dcrdex/server/coinlock"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/db"
	"decred.org/dcrdex/server/matcher"
)

var (
	// The coin waiter will query for transaction data every recheckInterval.
	recheckInterval = time.Second * 3
	// txWaitExpiration is the longest the Swapper will wait for a coin waiter.
	// This could be thought of as the maximum allowable backend latency.
	txWaitExpiration = 2 * time.Minute
)

func unixMsNow() time.Time {
	return time.Now().Truncate(time.Millisecond).UTC()
}

func makerTaker(isMaker bool) string {
	if isMaker {
		return "maker"
	}
	return "taker"
}

// AuthManager handles client-related actions, including authorization and
// communications.
type AuthManager interface {
	Route(string, func(account.AccountID, *msgjson.Message) *msgjson.Error)
	Auth(user account.AccountID, msg, sig []byte) error
	Sign(...msgjson.Signable) error
	Send(account.AccountID, *msgjson.Message) error
	Request(account.AccountID, *msgjson.Message, func(comms.Link, *msgjson.Message)) error
	RequestWithTimeout(user account.AccountID, req *msgjson.Message, handlerFunc func(comms.Link, *msgjson.Message),
		expireTimeout time.Duration, expireFunc func()) error
	SwapSuccess(user account.AccountID, mmid db.MarketMatchID, value uint64, refTime time.Time)
	Inaction(user account.AccountID, misstep auth.NoActionStep, mmid db.MarketMatchID, matchValue uint64, refTime time.Time, oid order.OrderID)
	RecordCancel(user account.AccountID, oid, target order.OrderID, t time.Time)
	RecordCompletedOrder(user account.AccountID, oid order.OrderID, t time.Time)
}

// Storage updates match data in what is presumably a database.
type Storage interface {
	db.SwapArchiver
	LastErr() error
	Fatal() <-chan struct{}
	InsertMatch(match *order.Match) error
	CancelOrder(*order.LimitOrder) error
	RevokeOrder(order.Order) (cancelID order.OrderID, t time.Time, err error)
	SetOrderCompleteTime(ord order.Order, compTimeMs int64) error
	GetStateHash() ([]byte, error)
	SetStateHash([]byte) error
}

// swapStatus is information related to the completion or incompletion of each
// sequential step of the atomic swap negotiation process. Each user has their
// own swapStatus.
type swapStatus struct {
	// The asset to which the user broadcasts their swap transaction.
	swapAsset   uint32
	redeemAsset uint32

	mtx sync.RWMutex
	// The time that the swap coordinator sees the transaction.
	swapTime time.Time
	swap     asset.Contract
	// The time that the transaction receives its SwapConf'th confirmation.
	swapConfirmed time.Time
	// The time that the swap coordinator sees the user's redemption
	// transaction.
	redeemTime time.Time
	redemption asset.Coin
}

func (ss *swapStatus) swapConfTime() time.Time {
	ss.mtx.RLock()
	defer ss.mtx.RUnlock()
	return ss.swapConfirmed
}

func (ss *swapStatus) redeemSeenTime() time.Time {
	ss.mtx.RLock()
	defer ss.mtx.RUnlock()
	return ss.redeemTime
}

// matchTracker embeds an order.Match and adds some data necessary for tracking
// the match negotiation.
type matchTracker struct {
	mtx sync.RWMutex // Match.Sigs and Match.Status
	*order.Match
	time        time.Time
	matchTime   time.Time
	makerStatus *swapStatus
	takerStatus *swapStatus
}

// A blockNotification is used internally when an asset.Backend reports a new
// block.
type blockNotification struct {
	time    time.Time
	assetID uint32
	err     error
}

// A stepActor is a structure holding information about one party of a match.
// stepActor is used with the stepInformation structure, which is used for
// sequencing swap negotiation.
type stepActor struct {
	user account.AccountID
	// swapAsset is the asset to which this actor broadcasts their swap tx.
	swapAsset uint32
	isMaker   bool
	order     order.Order
	// The swapStatus from the Match. Could be either the
	// (matchTracker).makerStatus or (matchTracker).takerStatus, depending on who
	// this actor is.
	status *swapStatus
}

// stepInformation holds information about the current state of the swap
// negotiation. A new stepInformation should be generated with (Swapper).step at
// every step of the negotiation process.
type stepInformation struct {
	match *matchTracker
	// The actor is the user info for the user who is expected to be broadcasting
	// a swap or redemption transaction next.
	actor stepActor
	// counterParty is the user that is not expected to be acting next.
	counterParty stepActor
	// asset is the asset backend for swapAsset.
	asset *asset.BackedAsset
	// isBaseAsset will be true if the current step involves a transaction on the
	// match market's base asset blockchain, false if on quote asset's blockchain.
	isBaseAsset bool
	step        order.MatchStatus
	nextStep    order.MatchStatus
	// checkVal holds the trade amount in units of the currently acting asset,
	// and is used to validate the swap transaction details.
	checkVal uint64
}

// LockableAsset pairs an Asset with a CoinLocker.
type LockableAsset struct {
	*asset.BackedAsset
	coinlock.CoinLocker // should be *coinlock.AssetCoinLocker
}

type orderSwapStat struct {
	SwapCount int
	OffBook   bool
	HasFailed bool
}

// orderSwapTracker facilitates cancellation rate computation without complex,
// costly, and frequent DB queries.
type orderSwapTracker struct {
	mtx          sync.Mutex
	orderMatches map[order.OrderID]*orderSwapStat
}

func newOrderSwapTracker() *orderSwapTracker {
	return &orderSwapTracker{
		orderMatches: make(map[order.OrderID]*orderSwapStat),
	}
}

// decrementActiveSwapCount decrements the number of active swaps for an order,
// returning a boolean indicating if the order is now considered complete, where
// complete means there are no more active swaps, the order is off-book, and the
// user is not responsible for any swap failures or premature unbooking of the
// order. If the number of active swaps is reduced to zero, the order is removed
// from the tracker, and repeated calls to decrementActiveSwapCount for the
// order will return true.
func (s *orderSwapTracker) decrementActiveSwapCount(ord order.Order, failed bool) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	oid := ord.ID()
	stat := s.orderMatches[oid]
	if stat == nil {
		log.Warnf("isOrderComplete: untracked order %v", oid)
		return true
	}

	stat.HasFailed = stat.HasFailed || failed

	stat.SwapCount--
	if stat.SwapCount != 0 {
		return false
	}

	// Remove the order's entry only if:
	// - it has never failed OR
	// - it is off-book, meaning it will never be seen again
	if !stat.HasFailed || stat.OffBook {
		delete(s.orderMatches, oid)
	}

	return stat.OffBook && !stat.HasFailed
}

// swapSuccess decrements the active swap counter for an order. The return value
// indicates if the order is considered successfully complete, which is a status
// that precludes cancellation of the order, or failure of any swaps involving
// the order on account of the user's (in)action. The order's failure and
// off-book flags are unchanged.
func (s *orderSwapTracker) swapSuccess(ord order.Order) bool {
	return s.decrementActiveSwapCount(ord, false)
}

// swapFailure decrements the active swap counter for an order, and flags the
// order as having failed. The off-book flag is unchanged.
func (s *orderSwapTracker) swapFailure(ord order.Order) {
	s.decrementActiveSwapCount(ord, true)
}

// incActiveSwapCount registers a new swap for the given order, flagging the
// order according to offBook.
func (s *orderSwapTracker) incActiveSwapCount(ord order.Order, offBook bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	oid := ord.ID()
	stat := s.orderMatches[oid]
	if stat == nil {
		s.orderMatches[oid] = &orderSwapStat{
			SwapCount: 1,
			OffBook:   offBook,
		}
		return
	}

	stat.SwapCount++
	stat.OffBook = offBook // should never change true to false
}

// canceled records an order as off-book and failed if it exists in the active
// order swaps map. No new entry is made because there cannot be future swaps
// for a canceled order. For this reason, be sure to use incActiveSwapCount
// before canceled when processing new matches in Negotiate.
func (s *orderSwapTracker) canceled(ord order.Order) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	oid := ord.ID()
	stat := s.orderMatches[oid]
	if stat == nil {
		// No active swaps for canceled order, OK.
		return
	}

	// Prevent completion of active swaps from counting the order as
	// successfully completed.
	stat.OffBook = true
	stat.HasFailed = true
}

// Swapper handles order matches by handling authentication and inter-party
// communications between clients, or 'users'. The Swapper authenticates users
// (vua AuthManager) and validates transactions as they are reported.
type Swapper struct {
	// Where state is dumped on shutdown.
	dataDir string
	// coins is a map to all the Asset information, including the asset backends,
	// used by this Swapper.
	coins map[uint32]*LockableAsset
	// storage is a Database backend.
	storage Storage
	// authMgr is an AuthManager for client messaging and authentication.
	authMgr AuthManager
	// unbookHook is a callback the the Swapper's controller (e.g. DEX manager)
	// for removing an order from the book. This should be called when a swap is
	// revoked due to failure of the order's owner to complete the necessary
	// actions.
	unbookHook func(lo *order.LimitOrder) bool
	// The matches map and the contained matches are protected by the matchMtx.
	matchMtx    sync.RWMutex
	matches     map[order.MatchID]*matchTracker
	userMatches map[account.AccountID]map[order.MatchID]*matchTracker
	// orders tracks order status and active swaps.
	orders *orderSwapTracker
	// The broadcast timeout.
	bTimeout time.Duration
	// Expected locktimes for maker and taker swaps.
	lockTimeTaker time.Duration
	lockTimeMaker time.Duration
	// latencyQ is a queue for coin waiters to deal with network latency.
	latencyQ *wait.TickerQueue

	// handlerMtx should be read-locked for the duration of the comms route
	// handlers (handleInit and handleRedeem) and Negotiate. This blocks
	// shutdown until any coin waiters are registered with latencyQ. It should
	// be write-locked before setting the stop flag.
	handlerMtx sync.RWMutex
	// stop is used to prevent new handlers from starting coin waiters. It is
	// set to true during shutdown of Run.
	stop bool

	// liveWaiters is used to track active coin waiters running in latencyQ.
	liveWaitersMtx sync.Mutex
	liveWaiters    map[waiterKey]*handlerArgs
}

// Config is the swapper configuration settings. A Config instance is the only
// argument to the Swapper constructor.
type Config struct {
	// DataDir is the folder where swap state is stored.
	DataDir string
	// AllowPartialRestore indicates if it is acceptable to load only part of
	// the State if the Swapper's asset configuration lacks assets required to
	// load all elements of State.
	AllowPartialRestore bool
	// Assets is a map to all the asset information, including the asset backends,
	// used by this Swapper.
	Assets map[uint32]*LockableAsset
	// AuthManager is the auth manager for client messaging and authentication.
	AuthManager AuthManager
	// A database backend.
	Storage Storage
	// BroadcastTimeout is how long the Swapper will wait for expected swap
	// transactions following new blocks.
	BroadcastTimeout time.Duration
	// LockTimeTaker is the locktime Swapper will use for auditing taker swaps.
	LockTimeTaker time.Duration
	// LockTimeTaker is the locktime Swapper will use for auditing maker swaps.
	LockTimeMaker time.Duration
	// IgnoreState indicates that the swapper should not load the latest state
	// from file.
	IgnoreState bool
	// StatePath is a path to a swap state file from which the swapper state
	// will be loaded. If StatePath is not set, and IgnoreState if false, the
	// most recent stored state file will be loaded. StatePath supercedes
	// IgnoreState.
	StatePath string
	// UnbookHook informs the DEX manager that the specified order should be
	// removed from the order book.
	UnbookHook func(lo *order.LimitOrder) bool
}

// NewSwapper is a constructor for a Swapper.
func NewSwapper(cfg *Config) (*Swapper, error) {
	// Verify the directory where swap state will be saved.
	inf, err := os.Stat(cfg.DataDir)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("data folder %q does not exist", cfg.DataDir)
	}
	if !inf.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", cfg.DataDir)
	}

	for _, asset := range cfg.Assets {
		if asset.MaxFeeRate == 0 {
			return nil, fmt.Errorf("max fee rate of 0 is invalid for asset %q", asset.Symbol)
		}
	}

	authMgr := cfg.AuthManager
	swapper := &Swapper{
		dataDir:       cfg.DataDir,
		coins:         cfg.Assets,
		storage:       cfg.Storage,
		authMgr:       authMgr,
		unbookHook:    cfg.UnbookHook,
		latencyQ:      wait.NewTickerQueue(recheckInterval),
		matches:       make(map[order.MatchID]*matchTracker),
		userMatches:   make(map[account.AccountID]map[order.MatchID]*matchTracker),
		orders:        newOrderSwapTracker(),
		bTimeout:      cfg.BroadcastTimeout,
		lockTimeTaker: cfg.LockTimeTaker,
		lockTimeMaker: cfg.LockTimeMaker,
		liveWaiters:   make(map[waiterKey]*handlerArgs),
	}

	// Ensure txWaitExpiration is not greater than broadcast timeout setting.
	if sensible := swapper.bTimeout; txWaitExpiration > sensible {
		txWaitExpiration = sensible
	}

	// Load the initial state.
	var state *State
	if cfg.StatePath != "" {
		log.Infof("attempting to load the swap state from user-specified file at %s", cfg.StatePath)
		state, err = LoadStateFile(cfg.StatePath)
		if err != nil {
			return nil, fmt.Errorf("failed to load specified state file %s", cfg.StatePath)
		}

	} else if !cfg.IgnoreState {
		log.Infof("searching for swap state files in %q", cfg.DataDir)
		stateFile, err := LatestStateFile(cfg.DataDir)
		if err != nil {
			return nil, fmt.Errorf("unable to read datadir: %w", err)
		}
		if stateFile != nil {
			// Get the last stored state file hash, and check it against the
			// most recent state file.
			stateHash, err := swapper.storage.GetStateHash()
			if err != nil {
				return nil, fmt.Errorf("error getting stateHash")
			}

			// If stateHash is empty, there has never been a state hash stored,
			// so there is nothing to do.
			if len(stateHash) > 0 {
				fileHash, err := encode.FileHash(stateFile.Name)
				if err != nil {
					return nil, fmt.Errorf("FileHash error: %w", err)
				}

				if !bytes.Equal(stateHash, fileHash) {
					return nil, fmt.Errorf("latest swap %s file failed consistency check", stateFile.Name)
				}

				state, err = LoadStateFile(stateFile.Name)
				if err != nil {
					return nil, fmt.Errorf("failed to load swap state file %v: %w", stateFile.Name, err)
				}
				log.Infof("loaded the most recent swap state file from %q", stateFile.Name)
			}

		} else {
			log.Info("no swap state files found")
		}
	}

	if state != nil {
		log.Infof("loaded swap state contains %d live matches and %d live coin waiters",
			len(state.MatchTrackers), len(state.LiveWaiters))
		err = swapper.restoreState(state, cfg.AllowPartialRestore)
		if err != nil {
			return nil, err
		}
	}

	// The swapper is only concerned with two types of client-originating
	// method requests.
	authMgr.Route(msgjson.InitRoute, swapper.handleInit)
	authMgr.Route(msgjson.RedeemRoute, swapper.handleRedeem)

	return swapper, nil
}

// addMatch registers a match. The matchMtx must be locked.
func (s *Swapper) addMatch(mt *matchTracker) {
	mid := mt.ID()
	s.matches[mid] = mt

	// Add the match to both maker's and taker's match maps.
	maker, taker := mt.Maker.User(), mt.Taker.User()
	for _, user := range []account.AccountID{maker, taker} {
		userMatches, found := s.userMatches[user]
		if !found {
			s.userMatches[user] = map[order.MatchID]*matchTracker{
				mid: mt,
			}
		} else {
			userMatches[mid] = mt // may overwrite for self-match (ok)
		}
		if maker == taker {
			break
		}
	}
}

// deleteMatch unregisters a match. The matchMtx must be locked.
func (s *Swapper) deleteMatch(mt *matchTracker) {
	mid := mt.ID()
	delete(s.matches, mid)

	// Remove the match from both maker's and taker's match maps.
	maker, taker := mt.Maker.User(), mt.Taker.User()
	for _, user := range []account.AccountID{maker, taker} {
		userMatches, found := s.userMatches[user]
		if !found {
			// Should not happen if consistently using addMatch.
			log.Errorf("deleteMatch: No matches for user %v found!", user)
			continue
		}
		delete(userMatches, mid)
		if len(userMatches) == 0 {
			delete(s.userMatches, user)
		}
		if maker == taker {
			break
		}
	}
}

// UserSwappingAmt gets the total amount in active swaps for a user in a
// specified market. This helps the market compute a user's order size limit.
func (s *Swapper) UserSwappingAmt(user account.AccountID, base, quote uint32) (amt, count uint64) {
	s.matchMtx.RLock()
	defer s.matchMtx.RUnlock()
	um, found := s.userMatches[user]
	if !found {
		return
	}
	for _, mt := range um {
		if mt.Maker.BaseAsset == base && mt.Maker.QuoteAsset == quote {
			amt += mt.Quantity
			count++
		}
	}
	return
}

// ChainsSynced will return true if both specified asset's backends are synced.
func (s *Swapper) ChainsSynced(base, quote uint32) (bool, error) {
	b, found := s.coins[base]
	if !found {
		return false, fmt.Errorf("No backend found for %d", base)
	}
	baseSynced, err := b.Backend.Synced()
	if err != nil {
		return false, fmt.Errorf("Error checking sync status for %d", base)
	}
	if !baseSynced {
		return false, nil
	}
	q, found := s.coins[quote]
	if !found {
		return false, fmt.Errorf("No backend found for %d", base)
	}
	quoteSynced, err := q.Backend.Synced()
	if err != nil {
		return false, fmt.Errorf("Error checking sync status for %d", base)
	}
	return quoteSynced, nil
}

func (s *Swapper) restoreState(state *State, allowPartial bool) error {
	// State binary version check should be done when State is loaded.

	// Check that the assets required by State are included
	missingAssets := make(map[uint32]bool)
	for _, id := range state.Assets {
		if s.coins[id] == nil {
			if !allowPartial {
				return fmt.Errorf("unable to find backend for asset %d in restore state", id)
			}
			log.Warnf("Unable to find backend for asset %d in restore state.", id)
			missingAssets[id] = true
		}
	}

	// Load the matchTrackers, calling the Contract and Redemption asset.Backend
	// methods as needed.

	translateSwapStatus := func(ss *swapStatus, ssd *swapStatusData, cpSwapCoin []byte) error {
		swapCoin := ssd.ContractCoinOut
		if len(swapCoin) > 0 {
			assetID := ssd.SwapAsset
			swap, err := s.coins[assetID].Backend.Contract(swapCoin, ssd.ContractScript)
			if err != nil {
				return fmt.Errorf("unable to find swap out coin %x for asset %d: %w", swapCoin, assetID, err)
			}
			ss.swap = swap
			ss.swapTime = encode.UnixTimeMilli(ssd.SwapTime)
		}

		if ssd.SwapConfirmTime != 0 {
			ss.swapConfirmed = encode.UnixTimeMilli(ssd.SwapConfirmTime)
		}

		if redeemCoin := ssd.RedeemCoinIn; len(redeemCoin) > 0 {
			assetID := ssd.RedeemAsset
			redeem, err := s.coins[assetID].Backend.Redemption(redeemCoin, cpSwapCoin)
			if err != nil {
				return fmt.Errorf("unable to find redeem in coin %x for asset %d: %w", redeemCoin, assetID, err)
			}
			ss.redemption = redeem
			ss.redeemTime = encode.UnixTimeMilli(ssd.RedeemTime)
		}

		return nil
	}

	s.matches = make(map[order.MatchID]*matchTracker, len(state.MatchTrackers))
	s.userMatches = make(map[account.AccountID]map[order.MatchID]*matchTracker)
	for mid, mtd := range state.MatchTrackers {
		// Check and skip matches for missing assets.
		makerSwapAsset := mtd.MakerStatus.SwapAsset
		makerRedeemAsset := mtd.MakerStatus.RedeemAsset
		if missingAssets[makerSwapAsset] {
			log.Infof("Skipping match %v with missing asset %d", mid, makerSwapAsset)
			continue
		}
		if missingAssets[makerRedeemAsset] {
			log.Infof("Skipping match %v with missing asset %d", mid, makerRedeemAsset)
			continue
		}

		mt := &matchTracker{
			Match:     mtd.Match,
			time:      encode.UnixTimeMilli(mtd.Time),
			matchTime: mtd.Match.Epoch.End(),
			makerStatus: &swapStatus{
				swapAsset:   makerSwapAsset,
				redeemAsset: makerRedeemAsset,
			},
			takerStatus: &swapStatus{
				swapAsset:   makerRedeemAsset, // mtd.TakerStatus.SwapAsset
				redeemAsset: makerSwapAsset,   // mtd.TakerStatus.RedeemAsset
			},
		}

		if err := translateSwapStatus(mt.makerStatus, mtd.MakerStatus, mtd.TakerStatus.ContractCoinOut); err != nil {
			log.Errorf("Loading match %v failed: %v", mtd.Match.ID(), err)
			continue
		}
		if err := translateSwapStatus(mt.takerStatus, mtd.TakerStatus, mtd.MakerStatus.ContractCoinOut); err != nil {
			log.Errorf("Loading match %v failed: %v", mtd.Match.ID(), err)
			continue
		}

		s.addMatch(mt)
	}

	// Order completion/failure tracking data
	s.orders.orderMatches = state.OrderMatches

	// Live coin waiters started by the comms handlers for client init and
	// redeem messages
	//
	// Rerun handleInit or handleRedeem to revalidate the contract/redeem and
	// start the coin waiters that trigger processInit and processRedeem.
	// Alternatively, we could manually start a waiter. Manually creating a
	// waiter requires (1) s.step to get stepInfo and (2) msg.Payload unmarshal
	// into params, a msgjson.Init or msgjson.Redeem. Manually doing this would
	// skip the msg and contract/redeem validation.

	for _, waitDat := range state.LiveWaiters {
		var msgErr *msgjson.Error
		rt := waitDat.Msg.Route
		switch rt {
		case msgjson.InitRoute:
			msgErr = s.handleInit(waitDat.User, waitDat.Msg)
		case msgjson.RedeemRoute:
			msgErr = s.handleRedeem(waitDat.User, waitDat.Msg)
		default:
			log.Errorf("%s is not a route that starts coinwaiters!", rt)
			continue
		}

		if msgErr != nil {
			log.Errorf("Failed to reprocess %v message: %v", rt, msgErr)
		}
	}

	return nil
}

func (s *Swapper) saveState() {
	// Store state.
	fName := fmt.Sprintf("swapState-%d.gob", encode.UnixMilli(time.Now()))
	fPath := filepath.Join(s.dataDir, fName)
	f, err := os.Create(fPath)
	if err != nil {
		log.Errorf("Failed to create swap state file %v: %v", fName, err)
		return
	}
	defer f.Close()

	mtd := make(map[order.MatchID]*matchTrackerData, len(s.matches))
	neededAssets := make(map[uint32]struct{}, len(s.coins))
	for matchID, mt := range s.matches {
		neededAssets[mt.Match.Maker.BaseAsset] = struct{}{}
		neededAssets[mt.Match.Maker.QuoteAsset] = struct{}{}
		mtd[matchID] = &matchTrackerData{
			Match:       mt.Match,
			Time:        encode.UnixMilli(mt.time),
			MakerStatus: mt.makerStatus.Data(),
			TakerStatus: mt.takerStatus.Data(),
		}
	}
	assetIDs := make([]uint32, 0, len(neededAssets))
	for id := range neededAssets {
		assetIDs = append(assetIDs, id)
	}

	st := &State{
		Version:       stateBinaryVersion,
		Assets:        assetIDs,
		MatchTrackers: mtd,
		OrderMatches:  s.orders.orderMatches,
		LiveWaiters:   s.liveWaiters,
	}

	enc := gob.NewEncoder(f)
	if err = enc.Encode(st); err != nil {
		log.Errorf("Failed to save swap state to file %v: %v", fName, err)
		return
	}

	if err = f.Sync(); err != nil {
		log.Errorf("Failed to write swap state data to disk: %v", err)
	} else {
		log.Infof("Saved swap state to file %q, containing %d live matches with "+
			"and %d live coin waiters", fName, len(st.MatchTrackers), len(st.LiveWaiters))
	}

	// Save the filehash of the state file for a consistency check on startup.
	fileHash, err := encode.FileHash(fPath)
	if err != nil {
		log.Errorf("error hashing swap state file: %v", err)
		return
	}

	err = s.storage.SetStateHash(fileHash)
	if err != nil {
		log.Errorf("error storing swap hash to disk: %v", err)
	}
}

// Data input to handleInit and handleRedeem, which both start coin waiters with
// latencyQ. Store handlerArgs before starting the waiter, and remove them when
// the waiter completes or expires. On Swapper shutdown, the stored handlerArgs
// should be saved in the state data after stopping the latencyQ.
type handlerArgs struct {
	User account.AccountID
	Msg  *msgjson.Message // init or redeem inferred from Msg.
}

// client generated message IDs are not globally unique.
type waiterKey struct {
	MsgID uint64
	User  account.AccountID
}

func (s *Swapper) setLiveWaiter(user account.AccountID, msg *msgjson.Message) {
	s.liveWaitersMtx.Lock()
	key := waiterKey{msg.ID, user}
	s.liveWaiters[key] = &handlerArgs{
		User: user,
		Msg:  msg,
	}
	s.liveWaitersMtx.Unlock()
}

func (s *Swapper) rmLiveWaiter(user account.AccountID, msgID uint64) {
	s.liveWaitersMtx.Lock()
	delete(s.liveWaiters, waiterKey{msgID, user})
	s.liveWaitersMtx.Unlock()
}

// Run is the main Swapper loop. It's primary purpose is to update transaction
// confirmations when new blocks are mined, and to trigger inaction checks.
func (s *Swapper) Run(ctx context.Context) {
	// Permit internal cancel on anomaly such as storage failure.
	ctxMaster, cancel := context.WithCancel(ctx)

	// Graceful shutdown first allows active incoming messages to be handled,
	// blocks more incoming messages in the handler functions, stops the helper
	// goroutines (latency queue used by the handlers, and the block ntfn
	// receiver), and finally the main loop via the mainLoop channel.
	var wgHelpers, wgMain sync.WaitGroup
	ctxHelpers, cancelHelpers := context.WithCancel(context.Background())
	mainLoop := make(chan struct{}) // close after helpers stop for graceful shutdown
	defer func() {
		// Stop handlers receiving messages and queueing latency Waiters.
		s.handlerMtx.Lock() // block until active handlers return
		s.stop = true       // prevent new handlers from starting waiters
		// NOTE: could also do authMgr.Route(msgjson.{InitRoute,RedeemRoute}, shuttingDownHandler)
		s.handlerMtx.Unlock()

		// Stop the latencyQ of Waiters and the block update goroutines that
		// send to the main loop.
		cancelHelpers()
		wgHelpers.Wait()

		// Now that handlers AND the coin waiter queue are stopped, the
		// liveWaiters can be accessed without locking.

		// Stop the main loop if if there was no internal error.
		close(mainLoop)
		wgMain.Wait()

		s.saveState()

	}()

	// Start a listen loop for each asset's block channel. Normal shutdown stops
	// this before the main loop since this sends to the main loop.
	blockNotes := make(chan *blockNotification, 32*len(s.coins))
	for assetID, lockable := range s.coins {
		wgHelpers.Add(1)
		go func(assetID uint32, blockSource <-chan *asset.BlockUpdate) {
			defer wgHelpers.Done()
			for {
				select {
				case blk, ok := <-blockSource:
					if !ok {
						log.Errorf("Asset %d has closed the block channel.", assetID)
						// Should not happen. Keep running until cancel.
						continue
					}
					// Do not block on anomalous return of main loop, which is
					// the s.block receiver.
					select {
					case <-mainLoop:
						return // ctxHelpers is being canceled anyway
					case blockNotes <- &blockNotification{
						time:    time.Now().UTC(),
						assetID: assetID,
						err:     blk.Err,
					}:
					}
				case <-ctxHelpers.Done():
					return
				}
			}
		}(assetID, lockable.Backend.BlockChannel(32))
	}

	// Start the queue of coinwaiters for the init and redeem handlers. The
	// handlers must be stopped/blocked before stopping this.
	wgHelpers.Add(1)
	go func() {
		s.latencyQ.Run(ctxHelpers)
		wgHelpers.Done()
	}()

	log.Debugf("Swapper started with %v broadcast timeout.", s.bTimeout)

	// Block-based inaction checks are started with Timers, and run in the main
	// loop to avoid locks and WaitGroups.
	bcastBlockTrigger := make(chan uint32, 32*len(s.coins))
	scheduleInactionCheck := func(assetID uint32) {
		time.AfterFunc(s.bTimeout, func() {
			select {
			case bcastBlockTrigger <- assetID: // all checks run in main loop
			case <-ctxMaster.Done():
			}
		})
	}

	// On startup, schedule an inaction check for each asset. Ideally these
	// would start bTimeout after the best block times.
	for assetID := range s.coins {
		scheduleInactionCheck(assetID)
	}

	// Event-based action checks are started with a single ticker. Each of the
	// events, e.g. match request, could start a timer, but this is simpler and
	// allows batching the match checks.
	bcastEventTrigger := bufferedTicker(ctxMaster, s.bTimeout/4)

	// Main loop can stop on internal error via cancel(), or when the caller
	// cancels the parent context triggering graceful shutdown.
	wgMain.Add(1)
	go func() {
		defer wgMain.Done()
		defer cancel() // ctxMaster for anomalous return
		for {
			select {
			case <-s.storage.Fatal():
				return
			case block := <-blockNotes:
				if block.err != nil {
					var connectionErr asset.ConnectionError
					if errors.As(block.err, &connectionErr) {
						// Connection issues handling can be triggered here.
						log.Errorf("connection error detected for %d: %v", block.assetID, block.err)
					} else {
						log.Errorf("asset %d is reporting a block notification error: %v", block.assetID, block.err)
					}
					continue
				}

				// processBlock will update confirmation times in the swapStatus
				// structs.
				//
				// WARNING! Confirmation checks in processBlock must be quick
				// because either of the blockNotification or asset.BlockUpdate
				// channels could fill up and we will miss blocks! The asset
				// backends should allow a blocking channel, just retrying on
				// the next block, but that is not a solution.
				//
				// TODO: Consider a timeout for the entire processBlock call:
				//
				//   ctxTime := context.WithTimeout(ctx, 2*time.Second)
				//   s.processBlock(ctxTime, block)
				//
				// Presently, one stuck backend that hangs on Confirmations
				// halts the whole DEX! So timeouts on Confirmations too.
				s.processBlock(block)

				// Schedule an inaction check for matches that involve this
				// asset, as they could be expecting user action within bTimeout
				// of this event.
				scheduleInactionCheck(block.assetID)

			case assetID := <-bcastBlockTrigger:
				// There was a new block for this asset bTimeout ago.
				s.checkInactionBlockBased(assetID)

			case <-bcastEventTrigger:
				// Inaction checks that are not relative to blocks.
				s.checkInactionEventBased()

			case <-mainLoop:
				return
			}
		}
	}()

	// Wait for caller cancel or anomalous return from main loop.
	<-ctxMaster.Done()
}

// bufferedTicker creates a "ticker" that periodically sends on the returned
// channel, which has a buffer of length 1 and thus suitable for use in a select
// with other events that might cause a regular Ticker send to be dropped.
func bufferedTicker(ctx context.Context, dur time.Duration) chan struct{} {
	buffered := make(chan struct{}, 1) // only need 1 since back-to-back is pointless
	go func() {
		ticker := time.NewTicker(dur)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				buffered <- struct{}{}
			case <-ctx.Done():
				return
			}
		}
	}()
	return buffered
}

func (s *Swapper) tryConfirmSwap(status *swapStatus, confTime time.Time) (final bool) {
	status.mtx.Lock()
	defer status.mtx.Unlock()
	if status.swapTime.IsZero() || !status.swapConfirmed.IsZero() {
		return
	}
	confs, err := status.swap.Confirmations()
	if err != nil {
		// The transaction has become invalid. No reason to do anything.
		return
	}
	// If a swapStatus was created, the asset.Asset is already known to be in
	// the map.
	swapConf := s.coins[status.swapAsset].SwapConf
	if confs >= int64(swapConf) {
		log.Debugf("Swap %v (%s) has reached %d confirmations (%d required)",
			status.swap, dex.BipIDSymbol(status.swapAsset), confs, swapConf)
		status.swapConfirmed = confTime.UTC()
		final = true
	}
	return
}

// processBlock scans the matches and updates match status based on number of
// confirmations. Once a relevant transaction has the requisite number of
// confirmations, the next-to-act has only duration (Swapper).bTimeout to
// broadcast the next transaction in the settlement sequence. The timeout is
// not evaluated here, but in (Swapper).checkInaction. This method simply sets
// the appropriate flags in the swapStatus structures.
func (s *Swapper) processBlock(block *blockNotification) {
	checkMatch := func(match *matchTracker) {
		// If it's neither of the match assets, nothing to do.
		if match.makerStatus.swapAsset != block.assetID &&
			match.takerStatus.swapAsset != block.assetID {
			return
		}

		// Lock the matchTracker so the following checks and updates are atomic
		// with respect to Status.
		match.mtx.RLock()
		defer match.mtx.RUnlock()

		switch match.Status {
		case order.MakerSwapCast:
			if match.makerStatus.swapAsset != block.assetID {
				break
			}
			// If the maker has broadcast their transaction, the taker's broadcast
			// timeout starts once the maker's swap has SwapConf confs.
			if s.tryConfirmSwap(match.makerStatus, block.time) {
				s.unlockOrderCoins(match.Maker)
			}
		case order.TakerSwapCast:
			if match.takerStatus.swapAsset != block.assetID {
				break
			}
			// If the taker has broadcast their transaction, the maker's broadcast
			// timeout (for redemption) starts once the maker's swap has SwapConf
			// confs.
			if s.tryConfirmSwap(match.takerStatus, block.time) {
				s.unlockOrderCoins(match.Taker)
			}
		}
	}

	s.matchMtx.Lock()
	defer s.matchMtx.Unlock()
	for _, match := range s.matches {
		checkMatch(match)
	}
}

func (s *Swapper) failMatch(match *matchTracker) {
	// From the match status, determine maker/taker fault and the corresponding
	// auth.NoActionStep.
	var makerFault bool
	var misstep auth.NoActionStep
	var refTime time.Time // a reference time found in the DB for reproducibly sorting outcomes
	switch match.Status {
	case order.NewlyMatched:
		misstep = auth.NoSwapAsMaker
		refTime = match.Epoch.End()
		makerFault = true
	case order.MakerSwapCast:
		misstep = auth.NoSwapAsTaker
		refTime = match.makerStatus.swapTime // swapConfirmed time is not in the DB
	case order.TakerSwapCast:
		misstep = auth.NoRedeemAsMaker
		refTime = match.takerStatus.swapTime // swapConfirmed time is not in the DB
		makerFault = true
	case order.MakerRedeemed:
		misstep = auth.NoRedeemAsTaker
		refTime = match.makerStatus.redeemTime
	default:
		log.Errorf("Invalid failMatch status %v for match %v", match.Status, match.ID())
		return
	}

	orderAtFault, otherOrder := match.Taker, order.Order(match.Maker) // an order.Order
	if makerFault {
		orderAtFault, otherOrder = match.Maker, match.Taker
	}
	log.Debugf("failMatch: swap %v failing (maker fault = %v) at %v",
		match.ID(), makerFault, match.Status)

	// Record the end of this match's processing.
	s.storage.SetMatchInactive(db.MatchID(match.Match))

	// If the at-fault order is a limit order, signal that if it is
	// still on the book it should be unbooked, changed to revoked
	// status, counted against the user's cancellation rate, and a
	// server-generated cancel order recorded.
	if lo, isLimit := orderAtFault.(*order.LimitOrder); isLimit {
		if s.unbookHook(lo) {
			s.orders.canceled(orderAtFault) // set as off-book and failed
		}
	}

	// That's one less active swap for this order, and a failure. If
	// this order has no other active swaps, it will be removed from the
	// order swap tracker by decrementActiveSwapCount since it is
	// off-book and no new swap negotiations can begin for this order.
	s.orders.swapFailure(orderAtFault)

	// The other order now has one less active swap too.
	if s.orders.swapSuccess(otherOrder) {
		// TODO: We should count this as a successful swap, but should
		// it only be a completed order with the extra stipulation that
		// it had already completed another swap?
		compTime := time.Now().UTC()
		s.authMgr.RecordCompletedOrder(otherOrder.User(), otherOrder.ID(), compTime)
		if err := s.storage.SetOrderCompleteTime(otherOrder, encode.UnixMilli(compTime)); err != nil {
			if db.IsErrGeneralFailure(err) {
				log.Errorf("fatal error with SetOrderCompleteTime for order %v: %v", otherOrder.UID(), err)
			} else {
				log.Warnf("SetOrderCompleteTime for %v: %v", otherOrder.UID(), err)
			}
		}
	}

	// Register the failure to act violation, adjusting the user's score.
	s.authMgr.Inaction(orderAtFault.User(), misstep, db.MatchID(match.Match), match.Quantity, refTime, orderAtFault.ID())

	// Send the revoke_match messages, and solicit acks.
	s.revoke(match)
}

// checkInactionEventBased scans the swapStatus structures, checking for actions
// that are expected in a time frame relative to another event that is not a
// confirmation time. If a client is found to have not acted when required, a
// match may be revoked and a penalty assigned to the user. This includes
// matches in NewlyMatched that have not received a maker swap following the
// match request, and in MakerRedeemed that have not received a taker redeem
// following the redemption request triggered by the makers redeem.
func (s *Swapper) checkInactionEventBased() {
	// If the DB is failing, do not penalize or attempt to start revocations.
	if err := s.storage.LastErr(); err != nil {
		log.Errorf("DB in failing state.")
		return
	}

	var deletions []*matchTracker

	// Do time.Since(event) with the same now time for each match.
	now := time.Now()
	tooOld := func(evt time.Time) bool {
		return now.Sub(evt) >= s.bTimeout
	}

	checkMatch := func(match *matchTracker) {
		// Lock entire matchTracker so the following is atomic with respect to
		// Status.
		match.mtx.RLock()
		defer match.mtx.RUnlock()

		log.Tracef("checkInactionEventBased: match %v (%v)", match.ID(), match.Status)

		failMatch := func() {
			s.failMatch(match)
			deletions = append(deletions, match)
		}

		switch match.Status {
		case order.NewlyMatched:
			// Maker has not broadcast their swap. They have until match time
			// plus bTimeout.
			if tooOld(match.time) {
				failMatch()
			}
		case order.MakerRedeemed:
			// If the maker has redeemed, the taker can redeem immediately, so
			// check the timeout against the time the Swapper received the
			// maker's `redeem` request (and sent the taker's 'redemption').
			if tooOld(match.makerStatus.redeemSeenTime()) {
				failMatch()
			}
		}
	}

	s.matchMtx.Lock()
	defer s.matchMtx.Unlock()

	for _, match := range s.matches {
		checkMatch(match)
	}

	for _, match := range deletions {
		s.deleteMatch(match)
	}
}

// checkInactionBlockBased scans the swapStatus structures relevant to the
// specified asset. If a client is found to have not acted when required, a
// match may be revoked and a penalty assigned to the user. This includes
// matches in MakerSwapCast that have not received a taker swap after the
// maker's swap reaches the required confirmation count, and in TakerSwapCast
// that have not received a maker redeem after the taker's swap reaches the
// required confirmation count.
func (s *Swapper) checkInactionBlockBased(assetID uint32) {
	// If the DB is failing, do not penalize or attempt to start revocations.
	if err := s.storage.LastErr(); err != nil {
		log.Errorf("DB in failing state.")
		return
	}

	var deletions []*matchTracker
	// Do time.Since(event) with the same now time for each match.
	now := time.Now()
	tooOld := func(evt time.Time) bool {
		// If the time is not set (zero), it has not happened yet (not too old).
		return !evt.IsZero() && now.Sub(evt) >= s.bTimeout
	}

	checkMatch := func(match *matchTracker) {
		if match.makerStatus.swapAsset != assetID && match.takerStatus.swapAsset != assetID {
			return
		}

		// Lock entire matchTracker so the following is atomic with respect to
		// Status.
		match.mtx.RLock()
		defer match.mtx.RUnlock()

		log.Tracef("checkInactionBlockBased: asset %d, match %v (%v)",
			assetID, match.ID(), match.Status)

		failMatch := func() {
			s.failMatch(match)
			deletions = append(deletions, match)
		}

		switch match.Status {
		case order.MakerSwapCast:
			if tooOld(match.makerStatus.swapConfTime()) {
				failMatch()
			}
		case order.TakerSwapCast:
			if tooOld(match.takerStatus.swapConfTime()) {
				failMatch()
			}
		}
	}

	s.matchMtx.Lock()
	defer s.matchMtx.Unlock()

	for _, match := range s.matches {
		checkMatch(match)
	}

	for _, match := range deletions {
		s.deleteMatch(match)
	}
}

// respondError sends an rpcError to a user.
func (s *Swapper) respondError(id uint64, user account.AccountID, code int, errMsg string) {
	log.Debugf("Error going to user %v, code: %d, msg: %s", user, code, errMsg)
	msg, err := msgjson.NewResponse(id, nil, &msgjson.Error{
		Code:    code,
		Message: errMsg,
	})
	if err != nil {
		log.Errorf("Failed to create error response with message '%s': %v", msg, err)
		return // this should not be possible, but don't pass nil msg to Send
	}
	if err := s.authMgr.Send(user, msg); err != nil {
		log.Infof("Unable to send %s error response (code = %d, msg = %s) to disconnected user %v: %q",
			msg.Route, code, errMsg, user, err)
	}
}

// respondSuccess sends a successful response to a user.
func (s *Swapper) respondSuccess(id uint64, user account.AccountID, result interface{}) {
	msg, err := msgjson.NewResponse(id, result, nil)
	if err != nil {
		log.Errorf("failed to send success: %v", err)
		return // this should not be possible, but don't pass nil msg to Send
	}
	if err := s.authMgr.Send(user, msg); err != nil {
		log.Infof("Unable to send %s success response to disconnected user %v: %v", msg.Route, user, err)
	}
}

// step creates a stepInformation structure for the specified match. A new
// stepInformation should be created for every client communication. The user
// is also validated as the actor. An error is returned if the user has not
// acknowledged their previous DEX requests.
func (s *Swapper) step(user account.AccountID, matchID order.MatchID) (*stepInformation, *msgjson.Error) {
	s.matchMtx.RLock()
	match, found := s.matches[matchID]
	s.matchMtx.RUnlock()
	if !found {
		return nil, &msgjson.Error{
			Code:    msgjson.RPCUnknownMatch,
			Message: "unknown match ID",
		}
	}

	// Get the step-related information for both parties.
	var isBaseAsset bool
	var actor, counterParty stepActor
	var nextStep order.MatchStatus
	maker, taker := match.Maker, match.Taker

	// Lock for Status and Sigs.
	match.mtx.RLock()
	defer match.mtx.RUnlock()

	// maker sell: base swap, quote redeem
	// taker buy: quote swap, base redeem

	// maker buy: quote swap, base redeem
	// taker sell: base swap, quote redeem

	// Maker broadcasts the swap contract. Sequence: NewlyMatched ->
	// MakerSwapCast -> TakerSwapCast -> MakerRedeemed -> MatchComplete
	switch match.Status {
	case order.NewlyMatched, order.TakerSwapCast:
		counterParty.order, actor.order = taker, maker
		actor.status = match.makerStatus
		counterParty.status = match.takerStatus
		actor.user = maker.User()
		counterParty.user = taker.User()
		actor.isMaker = true
		if match.Status == order.NewlyMatched {
			nextStep = order.MakerSwapCast
			isBaseAsset = maker.Sell // maker swap: base asset if sell
			if len(match.Sigs.MakerMatch) == 0 {
				log.Debugf("swap %v at status %v missing MakerMatch signature(s) expected before NewlyMatched->MakerSwapCast",
					match.ID(), match.Status)
			}
		} else /* TakerSwapCast */ {
			nextStep = order.MakerRedeemed
			isBaseAsset = !maker.Sell // maker redeem: base asset if buy
			if len(match.Sigs.MakerAudit) == 0 {
				log.Debugf("Swap %v at status %v missing MakerAudit signature(s) expected before TakerSwapCast->MakerRedeemed",
					match.ID(), match.Status)
			}
		}
	case order.MakerSwapCast, order.MakerRedeemed:
		counterParty.order, actor.order = maker, taker
		actor.status = match.takerStatus
		counterParty.status = match.makerStatus
		actor.user = taker.User()
		counterParty.user = maker.User()
		counterParty.isMaker = true
		if match.Status == order.MakerSwapCast {
			nextStep = order.TakerSwapCast
			isBaseAsset = !maker.Sell // taker swap: base asset if sell (maker buy)
			if len(match.Sigs.TakerMatch) == 0 {
				log.Debugf("Swap %v at status %v missing TakerMatch signature(s) expected before MakerSwapCast->TakerSwapCast",
					match.ID(), match.Status)
			}
			if len(match.Sigs.TakerAudit) == 0 {
				log.Debugf("Swap %v at status %v missing TakerAudit signature(s) expected before MakerSwapCast->TakerSwapCast",
					match.ID(), match.Status)
			}
		} else /* MakerRedeemed */ {
			nextStep = order.MatchComplete
			// Note that the swap is still considered "active" until both
			// counterparties acknowledge the redemptions.
			isBaseAsset = maker.Sell // taker redeem: base asset if buy (maker sell)
			if len(match.Sigs.TakerRedeem) == 0 {
				log.Debugf("Swap %v at status %v missing TakerRedeem signature(s) expected before MakerRedeemed->MatchComplete",
					match.ID(), match.Status)
			}
		}
	default:
		return nil, &msgjson.Error{
			Code:    msgjson.SettlementSequenceError,
			Message: "unknown settlement sequence identifier",
		}
	}

	// Verify that the user specified is the actor for this step.
	if actor.user != user {
		return nil, &msgjson.Error{
			Code:    msgjson.SettlementSequenceError,
			Message: "expected other party to act",
		}
	}

	// Set the actors' swapAsset and the swap contract checkVal.
	var checkVal uint64
	if isBaseAsset {
		actor.swapAsset = maker.BaseAsset
		counterParty.swapAsset = maker.QuoteAsset
		checkVal = match.Quantity
	} else {
		actor.swapAsset = maker.QuoteAsset
		counterParty.swapAsset = maker.BaseAsset
		checkVal = matcher.BaseToQuote(maker.Rate, match.Quantity)
	}

	return &stepInformation{
		match: match,
		actor: actor,
		// By the time a match is created, the presence of the asset in the map
		// has already been verified.
		asset:        s.coins[actor.swapAsset].BackedAsset,
		counterParty: counterParty,
		isBaseAsset:  isBaseAsset,
		step:         match.Status,
		nextStep:     nextStep,
		checkVal:     checkVal,
	}, nil
}

// authUser verifies that the msgjson.Signable is signed by the user. This
// method relies on the AuthManager to validate the signature of the serialized
// data. nil is returned for successful signature verification.
func (s *Swapper) authUser(user account.AccountID, params msgjson.Signable) *msgjson.Error {
	// Authorize the user.
	msg := params.Serialize()
	err := s.authMgr.Auth(user, msg, params.SigBytes())
	if err != nil {
		return &msgjson.Error{
			Code:    msgjson.SignatureError,
			Message: "error authenticating init params",
		}
	}
	return nil
}

// messageAcker is information needed to process the user's
// msgjson.Acknowledgement.
type messageAcker struct {
	user    account.AccountID
	match   *matchTracker
	params  msgjson.Signable
	isMaker bool
	isAudit bool
}

// processAck processes a msgjson.Acknowledgement to the audit, redemption, and
// revoke_match requests, validating the signature and updating the
// (order.Match).Sigs record. This is required by processInit, processRedeem,
// and revoke. Match Acknowledgements are handled by processMatchAck.
func (s *Swapper) processAck(msg *msgjson.Message, acker *messageAcker) {
	ack := new(msgjson.Acknowledgement)
	err := msg.UnmarshalResult(ack)
	if err != nil {
		s.respondError(msg.ID, acker.user, msgjson.RPCParseError, "error parsing acknowledgment")
		return
	}
	// Note: ack.MatchID unused, but could be checked against acker.match.ID().

	// Check the signature.
	sigMsg := acker.params.Serialize()
	err = s.authMgr.Auth(acker.user, sigMsg, ack.Sig)
	if err != nil {
		s.respondError(msg.ID, acker.user, msgjson.SignatureError,
			fmt.Sprintf("signature validation error: %v", err))
		return
	}

	switch acker.params.(type) {
	case *msgjson.Audit, *msgjson.Redemption:
	default:
		log.Warnf("unrecognized ack type %T", acker.params)
		return
	}

	// Set and store the appropriate signature, based on the current step and
	// actor.
	mktMatch := db.MatchID(acker.match.Match)

	acker.match.mtx.Lock()
	defer acker.match.mtx.Unlock()

	// This is an ack of either contract audit or redemption receipt.
	if acker.isAudit {
		log.Debugf("Received contract 'audit' acknowledgement from user %v (%s) for match %v (%v)",
			acker.user, makerTaker(acker.isMaker), acker.match.Match.ID(), acker.match.Status)
		// It's a contract audit ack.
		if acker.isMaker {
			acker.match.Sigs.MakerAudit = ack.Sig         // i.e. audited taker's contract
			s.storage.SaveAuditAckSigA(mktMatch, ack.Sig) // sql error makes backend go fatal
		} else {
			acker.match.Sigs.TakerAudit = ack.Sig
			s.storage.SaveAuditAckSigB(mktMatch, ack.Sig)
		}
		return
	}

	// It's a redemption ack.
	log.Debugf("Received 'redemption' acknowledgement from user %v (%s) for match %v (%s)",
		acker.user, makerTaker(acker.isMaker), acker.match.Match.ID(), acker.match.Status)

	// This is a redemption acknowledgement. Store the ack signature, and
	// potentially record the order as complete with the auth manager and in
	// persistent storage.

	// Record the taker's redeem ack sig. There isn't one for maker.
	if !acker.isMaker {
		acker.match.Sigs.TakerRedeem = ack.Sig
		if err = s.storage.SaveRedeemAckSigB(mktMatch, ack.Sig); err != nil {
			s.respondError(msg.ID, acker.user, msgjson.UnknownMarketError, "internal server error")
			log.Errorf("SaveRedeemAckSigB failed for match %v: %v", mktMatch.String(), err)
			return
		}
	}
}

// processInit processes the `init` RPC request, which is used to inform the DEX
// of a newly broadcast swap transaction. Once the transaction is seen and and
// audited by the Swapper, the counter-party is informed with an 'audit'
// request. This method is run as a coin waiter, hence the return value
// indicates if future attempts should be made to check coin status.
func (s *Swapper) processInit(msg *msgjson.Message, params *msgjson.Init, stepInfo *stepInformation) bool {
	// Validate the swap contract
	chain := stepInfo.asset.Backend
	actor, counterParty := stepInfo.actor, stepInfo.counterParty
	contract, err := chain.Contract(params.CoinID, params.Contract)
	if err != nil {
		if errors.Is(err, asset.CoinNotFoundError) {
			return wait.TryAgain
		}
		log.Warnf("Contract error encountered for match %s, actor %s using coin ID %x and contract %x: %v",
			stepInfo.match.ID(), actor.user, params.CoinID, params.Contract, err)
		s.respondError(msg.ID, actor.user, msgjson.ContractError,
			"redemption error")
		return wait.DontTryAgain
	}
	reqFeeRate := stepInfo.match.FeeRateQuote
	if stepInfo.isBaseAsset {
		reqFeeRate = stepInfo.match.FeeRateBase
	}
	if contract.FeeRate() < reqFeeRate {
		// TODO: test this case
		s.respondError(msg.ID, actor.user, msgjson.ContractError, "low tx fee")
		return wait.DontTryAgain
	}
	if contract.SwapAddress() != counterParty.order.Trade().SwapAddress() {
		s.respondError(msg.ID, actor.user, msgjson.ContractError,
			fmt.Sprintf("incorrect recipient. expected %s. got %s",
				contract.SwapAddress(), counterParty.order.Trade().SwapAddress()))
		return wait.DontTryAgain
	}
	if contract.Value() != stepInfo.checkVal {
		s.respondError(msg.ID, actor.user, msgjson.ContractError,
			fmt.Sprintf("contract error. expected contract value to be %d, got %d", stepInfo.checkVal, contract.Value()))
		return wait.DontTryAgain
	}

	reqLockTime := encode.DropMilliseconds(stepInfo.match.matchTime.Add(s.lockTimeTaker))
	if stepInfo.actor.isMaker {
		reqLockTime = encode.DropMilliseconds(stepInfo.match.matchTime.Add(s.lockTimeMaker))
	}
	if contract.LockTime().Before(reqLockTime) {
		s.respondError(msg.ID, actor.user, msgjson.ContractError,
			fmt.Sprintf("contract error. expected lock time >= %s, got %s", reqLockTime, contract.LockTime()))
		return wait.DontTryAgain
	}

	// Update the match.
	swapTime := unixMsNow()
	matchID := stepInfo.match.Match.ID()

	// Store the swap contract and the coinID (e.g. txid:vout) containing the
	// contract script hash. Maker is party A, the initiator. Taker is party B,
	// the participant.
	//
	// Failure to update match status is a fatal DB error. If we continue, the
	// swap may succeed, but there will be no way to recover or retrospectively
	// determine the swap outcome. Abort.
	storFn := s.storage.SaveContractB
	if stepInfo.actor.isMaker {
		storFn = s.storage.SaveContractA
	}
	mktMatch := db.MatchID(stepInfo.match.Match)
	swapTimeMs := encode.UnixMilli(swapTime)
	err = storFn(mktMatch, params.Contract, params.CoinID, swapTimeMs)
	if err != nil {
		log.Errorf("saving swap contract (match id=%v, maker=%v) failed: %v",
			matchID, actor.isMaker, err)
		s.respondError(msg.ID, actor.user, msgjson.UnknownMarketError,
			"internal server error")
		// TODO: revoke the match without penalties instead of retrying forever?
		return wait.TryAgain
	}

	// Modify the match's swapStatuses, but only if the match wasn't revoked
	// while waiting for the txn.
	s.matchMtx.RLock()
	if _, found := s.matches[matchID]; !found {
		s.matchMtx.RUnlock()
		log.Errorf("Contract txn located after match was revoked (match id=%v, maker=%v)",
			matchID, actor.isMaker)
		s.respondError(msg.ID, actor.user, msgjson.ContractError, "match already revoked due to inaction")
		return wait.DontTryAgain
	}

	actor.status.mtx.Lock()
	actor.status.swap = contract
	actor.status.swapTime = swapTime
	actor.status.mtx.Unlock()

	stepInfo.match.mtx.Lock()
	stepInfo.match.Status = stepInfo.nextStep
	stepInfo.match.mtx.Unlock()

	// Only unlock match map after the statuses and txn times are stored,
	// ensuring that checkInaction will not revoke the match as we respond and
	// request counterparty audit.
	s.matchMtx.RUnlock()

	log.Debugf("processInit: valid contract %v (%s) received at %v from user %v (%s) for match %v, "+
		"fee rate = %d, swapStatus %v => %v", contract, stepInfo.asset.Symbol, swapTime, actor.user,
		makerTaker(actor.isMaker), matchID, contract.FeeRate(), stepInfo.step, stepInfo.nextStep)

	// Issue a positive response to the actor.
	s.authMgr.Sign(params)
	s.respondSuccess(msg.ID, actor.user, &msgjson.Acknowledgement{
		MatchID: matchID[:],
		Sig:     params.Sig,
	})

	// Prepare an 'audit' request for the counter-party.
	auditParams := &msgjson.Audit{
		OrderID:  idToBytes(counterParty.order.ID()),
		MatchID:  matchID[:],
		Time:     uint64(swapTimeMs),
		CoinID:   params.CoinID,
		Contract: params.Contract,
	}
	s.authMgr.Sign(auditParams)
	notification, err := msgjson.NewRequest(comms.NextID(), msgjson.AuditRoute, auditParams)
	if err != nil {
		// This is likely an impossibly condition.
		log.Errorf("error creating audit request: %v", err)
		return wait.DontTryAgain
	}

	// Set up the acknowledgement for the callback.
	ack := &messageAcker{
		user:    counterParty.user,
		match:   stepInfo.match,
		params:  auditParams,
		isMaker: counterParty.isMaker,
		isAudit: true,
	}
	// Send the 'audit' request to the counter-party.
	log.Debugf("processInit: sending contract 'audit' request to counterparty %v (%s) "+
		"for match %v", ack.user, makerTaker(ack.isMaker), matchID)
	// Send the request.

	// The counterparty will audit the contract by retrieving it, which may
	// involve them waiting for up to the broadcast timeout before responding,
	// so the user gets at least s.bTimeout to the request.
	s.authMgr.RequestWithTimeout(ack.user, notification, func(_ comms.Link, resp *msgjson.Message) {
		s.processAck(resp, ack) // resp.ID == notification.ID
	}, s.bTimeout, func() {
		log.Infof("Timeout waiting for contract 'audit' request acknowledgement from user %v (%s) for match %v",
			ack.user, makerTaker(ack.isMaker), matchID)
	})

	return wait.DontTryAgain
}

// processRedeem processes a 'redeem' request from a client. processRedeem does
// not perform user authentication, which is handled in handleRedeem before
// processRedeem is invoked. This method is run as a coin waiter.
func (s *Swapper) processRedeem(msg *msgjson.Message, params *msgjson.Redeem, stepInfo *stepInformation) bool {
	// TODO(consider): Extract secret from initiator's (maker's) redemption
	// transaction. The Backend would need a method identify the component of
	// the redemption transaction that contains the secret and extract it. In a
	// UTXO-based asset, this means finding the input that spends the output of
	// the counterparty's contract, and process that input's signature script
	// with FindKeyPush. Presently this is up to the clients and not stored with
	// the server.

	// Make sure that the expected output is being spent.
	actor, counterParty := stepInfo.actor, stepInfo.counterParty
	counterParty.status.mtx.RLock()
	cpContract := counterParty.status.swap.RedeemScript()
	cpSwapCoin := counterParty.status.swap.ID()
	cpSwapStr := counterParty.status.swap.String()
	counterParty.status.mtx.RUnlock()

	// Get the transaction.
	match := stepInfo.match
	matchID := match.ID()
	chain := stepInfo.asset.Backend
	if !chain.ValidateSecret(params.Secret, cpContract) {
		log.Errorf("Secret validation failed (match id=%v, maker=%v, secret=%x)",
			matchID, actor.isMaker, params.Secret)
		s.respondError(msg.ID, actor.user, msgjson.UnknownMarketError, "secret validation failed")
		return wait.DontTryAgain
	}
	redemption, err := chain.Redemption(params.CoinID, cpSwapCoin)
	// If there is an error, don't return an error yet, since it could be due to
	// network latency. Instead, queue it up for another check.
	if err != nil {
		if errors.Is(err, asset.CoinNotFoundError) {
			return wait.TryAgain
		}
		log.Warnf("Redemption error encountered for match %s, actor %s, using coin ID %v to satisfy contract at %x: %v",
			stepInfo.match.ID(), actor, params.CoinID, cpSwapCoin, err)
		s.respondError(msg.ID, actor.user, msgjson.RedemptionError, "redemption error")
		return wait.DontTryAgain
	}

	newStatus := stepInfo.nextStep

	// NOTE: redemption.FeeRate is not checked since the counter party is not
	// inconvenienced by slow confirmation of the redemption.

	// Modify the match's swapStatuses, but only if the match wasn't revoked
	// while waiting for the txn.
	s.matchMtx.RLock()
	if _, found := s.matches[matchID]; !found {
		s.matchMtx.RUnlock()
		log.Errorf("Redeem txn found after match was revoked (match id=%v, maker=%v)",
			matchID, actor.isMaker)
		s.respondError(msg.ID, actor.user, msgjson.RedemptionError, "match already revoked due to inaction")
		return wait.DontTryAgain
	}

	actor.status.mtx.Lock()
	redeemTime := unixMsNow()
	actor.status.redemption = redemption
	actor.status.redeemTime = redeemTime
	actor.status.mtx.Unlock()

	match.mtx.Lock()
	match.Status = newStatus
	match.mtx.Unlock()

	// Only unlock match map after the statuses and txn times are stored,
	// ensuring that checkInaction will not revoke the match as we respond.
	s.matchMtx.RUnlock()

	log.Debugf("processRedeem: valid redemption %v (%s) spending contract %s received at %v from %v (%s) for match %v, "+
		"swapStatus %v => %v", redemption, stepInfo.asset.Symbol, cpSwapStr, redeemTime, actor.user,
		makerTaker(actor.isMaker), matchID, stepInfo.step, newStatus)

	if newStatus == order.MatchComplete { // actor is taker
		log.Debugf("Deleting completed match %v", matchID)
		s.matchMtx.Lock()
		s.deleteMatch(match)
		s.matchMtx.Unlock()
		// SaveRedeemB flags the match as inactive in the DB.
	}

	// Credit the user for completing the swap, adjusting the user's score.
	if actor.user != counterParty.user || newStatus == order.MatchComplete { // is user is both sides, only credit on MatchComplete (taker done too)
		s.authMgr.SwapSuccess(actor.user, db.MatchID(match.Match), match.Quantity, redeemTime)
	}

	// Store the swap contract and the coinID (e.g. txid:vout) containing the
	// contract script hash. Maker is party A, the initiator, who first reveals
	// the secret. Taker is party B, the participant.
	storFn := s.storage.SaveRedeemB // taker's redeem also sets match status to MatchComplete, active to FALSE
	if actor.isMaker {
		// Maker redeem stores the secret too.
		storFn = func(mid db.MarketMatchID, coinID []byte, timestamp int64) error {
			return s.storage.SaveRedeemA(mid, coinID, params.Secret, timestamp) // also sets match status to MakerRedeemed
		}
	}

	redeemTimeMs := encode.UnixMilli(redeemTime)
	err = storFn(db.MatchID(match.Match), params.CoinID, redeemTimeMs)
	if err != nil {
		log.Errorf("saving redeem transaction (match id=%v, maker=%v) failed: %v",
			matchID, actor.isMaker, err)
		// Neither party's fault. Continue.
	}

	// Issue a positive response to the actor.
	s.authMgr.Sign(params)
	s.respondSuccess(msg.ID, actor.user, &msgjson.Acknowledgement{
		MatchID: matchID[:],
		Sig:     params.Sig,
	})

	// Cancellation rate accounting
	ord := match.Taker
	if actor.isMaker {
		ord = match.Maker
	}
	// Mark the completed swap for this order.
	if s.orders.swapSuccess(ord) {
		// If it was the last for the order, AND no prior swaps for this order
		// failed with this user at-fault, register the order as successfully
		// completed in the auth manager.
		s.authMgr.RecordCompletedOrder(actor.user, ord.ID(), redeemTime)
		// Record the successful completion time.
		if err = s.storage.SetOrderCompleteTime(ord, redeemTimeMs); err != nil {
			if db.IsErrGeneralFailure(err) {
				log.Errorf("fatal error with SetOrderCompleteTime for order %v: %v", ord, err)
				s.respondError(msg.ID, actor.user, msgjson.UnknownMarketError, "internal server error")
				return wait.DontTryAgain
			}
			log.Errorf("SetOrderCompleteTime for %v: %v", ord, err)
		}
	}

	// For taker's redeem, that's the end.
	if !actor.isMaker {
		return wait.DontTryAgain
	}
	// For maker's redeem, inform the taker.
	rParams := &msgjson.Redemption{
		Redeem: msgjson.Redeem{
			OrderID: idToBytes(counterParty.order.ID()),
			MatchID: matchID[:],
			CoinID:  params.CoinID,
			Secret:  params.Secret,
		},
		Time: uint64(redeemTimeMs),
	}
	s.authMgr.Sign(rParams)
	redemptionReq, err := msgjson.NewRequest(comms.NextID(), msgjson.RedemptionRoute, rParams)
	if err != nil {
		log.Errorf("error creating redemption request: %v", err)
		return wait.DontTryAgain
	}

	// Set up the acknowledgement callback.
	ack := &messageAcker{
		user:    counterParty.user,
		match:   match,
		params:  rParams,
		isMaker: counterParty.isMaker,
		// isAudit: false,
	}
	log.Debugf("processRedeem: sending 'redemption' request to counterparty %v (%s) "+
		"for match %v", ack.user, makerTaker(ack.isMaker), matchID)

	// Send the ack request.

	// The counterparty does not need to actually locate the redemption txn,
	// so use the default request timeout.
	s.authMgr.RequestWithTimeout(ack.user, redemptionReq, func(_ comms.Link, resp *msgjson.Message) {
		s.processAck(resp, ack) // resp.ID == notification.ID
	}, time.Until(redeemTime.Add(s.bTimeout)), func() {
		log.Infof("Timeout waiting for 'redemption' request from user %v (%s) for match %v",
			ack.user, makerTaker(ack.isMaker), matchID)
	})

	return wait.DontTryAgain
}

// handleInit handles the 'init' request from a user, which is used to inform
// the DEX of a newly broadcast swap transaction. The Init message includes the
// swap contract script and the CoinID of contract. Most of the work is
// performed by processInit, but the request is parsed and user is authenticated
// first.
func (s *Swapper) handleInit(user account.AccountID, msg *msgjson.Message) *msgjson.Error {
	s.handlerMtx.RLock()
	defer s.handlerMtx.RUnlock() // block shutdown until registered with latencyQ
	if s.stop {
		return &msgjson.Error{
			Code:    msgjson.TryAgainLaterError,
			Message: "The swapper is stopping. Try again later.",
		}
	}

	params := new(msgjson.Init)
	err := json.Unmarshal(msg.Payload, &params)
	if err != nil {
		return &msgjson.Error{
			Code:    msgjson.RPCParseError,
			Message: "Error decoding 'init' method params",
		}
	}

	// Verify the user's signature of params.
	rpcErr := s.authUser(user, params)
	if rpcErr != nil {
		return rpcErr
	}

	log.Debugf("handleInit: 'init' received from user %v for match %v, order %v",
		user, params.MatchID, params.OrderID)

	if len(params.MatchID) != order.MatchIDSize {
		return &msgjson.Error{
			Code:    msgjson.RPCParseError,
			Message: "Invalid 'matchid' in 'init' message",
		}
	}

	var matchID order.MatchID
	copy(matchID[:], params.MatchID)
	stepInfo, rpcErr := s.step(user, matchID)
	if rpcErr != nil {
		return rpcErr
	}

	// init requests should only be sent when contracts are still required, in
	// the correct sequence, and by the correct party.
	switch stepInfo.match.Status {
	case order.NewlyMatched, order.MakerSwapCast:
	default:
		return &msgjson.Error{
			Code:    msgjson.SettlementSequenceError,
			Message: "swap contract already provided",
		}
	}

	// Validate the coinID and contract script before starting a coin waiter.
	coinStr, err := stepInfo.asset.Backend.ValidateCoinID(params.CoinID)
	if err != nil {
		// TODO: ensure Backends provide sanitized errors or type information to
		// provide more details to the client.
		return &msgjson.Error{
			Code:    msgjson.ContractError,
			Message: "invalid contract coinID or script",
		}
	}
	err = stepInfo.asset.Backend.ValidateContract(params.Contract)
	if err != nil {
		log.Debugf("ValidateContract (asset %v, coin %v) failure: %v", stepInfo.asset.Symbol, coinStr, err)
		// TODO: ensure Backends provide sanitized errors or type information to
		// provide more details to the client.
		return &msgjson.Error{
			Code:    msgjson.ContractError,
			Message: "invalid swap contract",
		}
	}

	// TODO: consider also checking recipient of contract here, but it is also
	// checked in processInit. Note that value cannot be checked as transaction
	// details, which includes the coin/output value, are not yet retrieved.

	s.setLiveWaiter(user, msg)

	// Do not search for the transaction past the inaction deadline. For maker,
	// this is bTimeout after match request. For taker, this is bTimeout after
	// maker's swap reached swapConfs.
	lastEvent := stepInfo.match.time // NewlyMatched - the match request time, not matchTime
	if stepInfo.step == order.MakerSwapCast {
		lastEvent = stepInfo.match.makerStatus.swapConfTime()
	}
	expireTime := time.Now().Add(txWaitExpiration).UTC()
	if lastEvent.IsZero() {
		log.Warnf("Prematurely received 'init' from %v at step %v", makerTaker(stepInfo.actor.isMaker), stepInfo.step)
	} else if deadline := lastEvent.Add(s.bTimeout); expireTime.After(deadline) {
		expireTime = deadline
	}
	log.Debugf("Allowing until %v (%v) to locate contract from %v (%v), match %v",
		expireTime, time.Until(expireTime), makerTaker(stepInfo.actor.isMaker),
		stepInfo.step, matchID)

	// Since we have to consider broadcast latency of the asset's network, run
	// this as a coin waiter.
	s.latencyQ.Wait(&wait.Waiter{
		Expiration: expireTime,
		TryFunc: func() bool {
			done := s.processInit(msg, params, stepInfo)
			if done == wait.DontTryAgain {
				// It is now either a live acker, or terminally failed.
				s.rmLiveWaiter(user, msg.ID)
			}
			return done
		},
		ExpireFunc: func() {
			s.rmLiveWaiter(user, msg.ID)
			// NOTE: We may consider a shorter expire time so the client can
			// receive warning that there may be node or wallet connectivity
			// trouble while they still have a chance to fix it.
			s.respondError(msg.ID, user, msgjson.TransactionUndiscovered,
				fmt.Sprintf("failed to find contract coin %v", coinStr))
		},
	})
	return nil
}

// handleRedeem handles the 'redeem' request from a user. Most of the work is
// performed by processRedeem, but the request is parsed and user is
// authenticated first.
func (s *Swapper) handleRedeem(user account.AccountID, msg *msgjson.Message) *msgjson.Error {
	s.handlerMtx.RLock()
	defer s.handlerMtx.RUnlock() // block shutdown until registered with latencyQ
	if s.stop {
		return &msgjson.Error{
			Code:    msgjson.TryAgainLaterError,
			Message: "The swapper is stopping. Try again later.",
		}
	}

	params := new(msgjson.Redeem)

	err := json.Unmarshal(msg.Payload, &params)
	if err != nil {
		return &msgjson.Error{
			Code:    msgjson.RPCParseError,
			Message: "Error decoding 'redeem' request payload",
		}
	}

	rpcErr := s.authUser(user, params)
	if rpcErr != nil {
		return rpcErr
	}

	log.Debugf("handleRedeem: 'redeem' received from %v for match %v, order %v",
		user, params.MatchID, params.OrderID)

	if len(params.MatchID) != order.MatchIDSize {
		return &msgjson.Error{
			Code:    msgjson.RPCParseError,
			Message: "Invalid 'matchid' in 'redeem' message",
		}
	}

	var matchID order.MatchID
	copy(matchID[:], params.MatchID)
	stepInfo, rpcErr := s.step(user, matchID)
	if rpcErr != nil {
		return rpcErr
	}
	// Validate the redeem coin ID before starting a wait. This does not
	// check the blockchain, but does ensure the CoinID can be decoded for the
	// asset before starting up a coin waiter.
	coinStr, err := stepInfo.asset.Backend.ValidateCoinID(params.CoinID)
	if err != nil {
		// TODO: ensure Backends provide sanitized errors or type information to
		// provide more details to the client.
		return &msgjson.Error{
			Code:    msgjson.ContractError,
			Message: "invalid 'redeem' parameters",
		}
	}

	s.setLiveWaiter(user, msg)

	// Do not search for the transaction past the inaction deadline. For maker,
	// this is bTimeout after taker's swap reached swapConfs. For taker, this is
	// bTimeout after maker's redeem cast (and redemption request time).
	lastEvent := stepInfo.match.takerStatus.swapConfTime() // TakerSwapCast
	if stepInfo.step == order.MakerRedeemed {
		lastEvent = stepInfo.match.makerStatus.redeemSeenTime()
	}
	expireTime := time.Now().Add(txWaitExpiration).UTC()
	if lastEvent.IsZero() {
		log.Warnf("Prematurely received 'redeem' from %v at step %v", makerTaker(stepInfo.actor.isMaker), stepInfo.step)
	} else if deadline := lastEvent.Add(s.bTimeout); expireTime.After(deadline) {
		expireTime = deadline
	}
	log.Debugf("Allowing until %v (%v) to locate redeem from %v (%v), match %v",
		expireTime, time.Until(expireTime), makerTaker(stepInfo.actor.isMaker),
		stepInfo.step, matchID)

	// Since we have to consider latency, run this as a coin waiter.
	s.latencyQ.Wait(&wait.Waiter{
		Expiration: expireTime,
		TryFunc: func() bool {
			done := s.processRedeem(msg, params, stepInfo)
			if done == wait.DontTryAgain {
				// It is now either a live acker, or terminally failed.
				s.rmLiveWaiter(user, msg.ID)
			}
			return done
		},
		ExpireFunc: func() {
			s.rmLiveWaiter(user, msg.ID)
			// NOTE: We may consider a shorter expire time so the client can
			// receive warning that there may be node or wallet connectivity
			// trouble while they still have a chance to fix it.
			s.respondError(msg.ID, user, msgjson.TransactionUndiscovered,
				fmt.Sprintf("failed to find redeemed coin %v", coinStr))
		},
	})
	return nil
}

// revoke revokes the match, sending the 'revoke_match' request to each client
// and processing the acknowledgement. Match Sigs and Status are not accessed.
func (s *Swapper) revoke(match *matchTracker) {
	route := msgjson.RevokeMatchRoute
	log.Infof("Sending a '%s' notification to each client for match %v",
		route, match.ID())
	// Unlock the maker and taker order coins.
	s.unlockOrderCoins(match.Taker)
	s.unlockOrderCoins(match.Maker)

	sendRev := func(mid order.MatchID, ord order.Order) {
		msg := &msgjson.RevokeMatch{
			OrderID: ord.ID().Bytes(),
			MatchID: mid[:],
		}
		s.authMgr.Sign(msg)
		ntfn, err := msgjson.NewNotification(route, msg)
		if err != nil {
			log.Errorf("Failed to create '%s' notification for user %v, match %v: %v",
				route, ord.User(), mid, err)
			return
		}
		if err = s.authMgr.Send(ord.User(), ntfn); err != nil {
			log.Debugf("Failed to send '%s' notification to user %v, match %v: %v",
				route, ord.User(), mid, err)
		}
	}

	mid := match.ID()
	sendRev(mid, match.Taker)
	sendRev(mid, match.Maker)
}

// extractAddress extracts the address from the order. If the order is a cancel
// order, an empty string is returned.
func extractAddress(ord order.Order) string {
	trade := ord.Trade()
	if trade == nil {
		return ""
	}
	return trade.Address
}

// For the 'match' request, the user returns a msgjson.Acknowledgement array
// with signatures for each match ID. The match acknowledgements were requested
// from each matched user in Negotiate.
func (s *Swapper) processMatchAcks(user account.AccountID, msg *msgjson.Message, matches []*messageAcker) {
	// NOTE: acks must be in same order as matches []*messageAcker.
	var acks []msgjson.Acknowledgement
	err := msg.UnmarshalResult(&acks)
	if err != nil {
		s.respondError(msg.ID, user, msgjson.RPCParseError,
			"error parsing match request acknowledgment")
		return
	}
	if len(matches) != len(acks) {
		s.respondError(msg.ID, user, msgjson.AckCountError,
			fmt.Sprintf("expected %d acknowledgements, got %d", len(acks), len(matches)))
		return
	}

	log.Debugf("processMatchAcks: 'match' ack received from %v for %d matches",
		user, len(matches))

	// Verify the signature of each Acknowledgement, and store the signatures in
	// the matchTracker of each match (messageAcker). The signature will be
	// either a MakerMatch or TakerMatch signature depending on whether the
	// responding user is the maker or taker.
	for i, matchInfo := range matches {
		ack := &acks[i]
		match := matchInfo.match

		matchID := match.ID()
		if !bytes.Equal(ack.MatchID, matchID[:]) {
			s.respondError(msg.ID, user, msgjson.IDMismatchError,
				fmt.Sprintf("unexpected match ID at acknowledgment index %d", i))
			return
		}
		sigMsg := matchInfo.params.Serialize()
		err = s.authMgr.Auth(user, sigMsg, ack.Sig)
		if err != nil {
			log.Warnf("processMatchAcks: 'match' ack for match %v from user %v, "+
				" failed sig verification: %v", matchID, user, err)
			s.respondError(msg.ID, user, msgjson.SignatureError,
				fmt.Sprintf("signature validation error: %v", err))
			return
		}

		// Store the signature in the matchTracker. These must be collected
		// before the init steps begin and swap contracts are broadcasted.
		match.mtx.Lock()
		log.Debugf("processMatchAcks: storing valid 'match' ack signature from %v (maker=%v) "+
			"for match %v (status %v)", user, matchInfo.isMaker, matchID, match.Status)
		if matchInfo.isMaker {
			match.Sigs.MakerMatch = ack.Sig
		} else {
			match.Sigs.TakerMatch = ack.Sig
		}
		match.mtx.Unlock()

	}

	// Store the signatures in the DB.
	for i, matchInfo := range matches {
		ackSig := acks[i].Sig
		match := matchInfo.match

		storFn := s.storage.SaveMatchAckSigB
		if matchInfo.isMaker {
			storFn = s.storage.SaveMatchAckSigA
		}
		matchID := match.ID()
		mid := db.MarketMatchID{
			MatchID: matchID,
			Base:    match.Maker.BaseAsset, // same for taker's redeem as BaseAsset refers to the market
			Quote:   match.Maker.QuoteAsset,
		}
		err = storFn(mid, ackSig)
		if err != nil {
			log.Errorf("saving match ack signature (match id=%v, maker=%v) failed: %v",
				matchID, matchInfo.isMaker, err)
			s.respondError(msg.ID, matchInfo.user, msgjson.UnknownMarketError,
				"internal server error")
			// TODO: revoke the match without penalties?
			return
		}
	}
}

// CheckUnspent attempts to verify a coin ID for a given asset by retrieving the
// corresponding asset.Coin. If the coin is not found or spent, an
// asset.CoinNotFoundError is returned.
func (s *Swapper) CheckUnspent(asset uint32, coinID []byte) error {
	backend := s.coins[asset]
	if backend == nil {
		return fmt.Errorf("unknown asset %d", asset)
	}

	return backend.Backend.VerifyUnspentCoin(coinID)
}

// LockOrdersCoins locks the backing coins for the provided orders.
func (s *Swapper) LockOrdersCoins(orders []order.Order) {
	// Separate orders according to the asset of their locked coins.
	assetCoinOrders := make(map[uint32][]order.Order, len(orders))
	for _, ord := range orders {
		// Identify the asset of the locked coins.
		asset := ord.Quote()
		if ord.Trade().Sell {
			asset = ord.Base()
		}
		assetCoinOrders[asset] = append(assetCoinOrders[asset], ord)
	}

	for asset, orders := range assetCoinOrders {
		s.lockOrdersCoins(asset, orders)
	}
}

func (s *Swapper) lockOrdersCoins(asset uint32, orders []order.Order) {
	assetLock := s.coins[asset]
	if assetLock == nil {
		panic(fmt.Sprintf("Unable to lock coins for asset %d", asset))
	}

	assetLock.LockOrdersCoins(orders)
}

// LockCoins locks coins of a given asset. The OrderID is used for tracking.
func (s *Swapper) LockCoins(asset uint32, coins map[order.OrderID][]order.CoinID) {
	assetLock := s.coins[asset]
	if assetLock == nil {
		panic(fmt.Sprintf("Unable to lock coins for asset %d", asset))
	}

	assetLock.LockCoins(coins)
}

// unlockOrderCoins is not exported since only the Swapper knows when to unlock
// coins (when funding coins are spent in a fully-confirmed contract).
func (s *Swapper) unlockOrderCoins(ord order.Order) {
	asset := ord.Quote()
	if ord.Trade().Sell {
		asset = ord.Base()
	}

	s.unlockOrderIDCoins(asset, ord.ID())
}

func (s *Swapper) unlockOrderIDCoins(asset uint32, oid order.OrderID) {
	assetLock := s.coins[asset]
	if assetLock == nil {
		panic(fmt.Sprintf("Unable to lock coins for asset %d", asset))
	}

	assetLock.UnlockOrderCoins(oid)
}

// matchNotifications creates a pair of msgjson.Match from a matchTracker.
func matchNotifications(match *matchTracker) (makerMsg *msgjson.Match, takerMsg *msgjson.Match) {
	// NOTE: If we decide that msgjson.Match should just have a
	// "FeeRateBaseSwap" field, this could be set according to the
	// swapStatus.swapAsset field:
	//
	// base, quote := match.Maker.BaseAsset, match.Maker.QuoteAsset
	// feeRate := func(assetID uint32) uint64 {
	// 	if assetID == match.Maker.BaseAsset {
	// 		return match.FeeRateBase
	// 	}
	// 	return match.FeeRateQuote
	// }
	// FeeRateMakerSwap := feeRate(match.makerStatus.swapAsset)

	stamp := encode.UnixMilliU(match.matchTime)
	return &msgjson.Match{
			OrderID:      idToBytes(match.Maker.ID()),
			MatchID:      idToBytes(match.ID()),
			Quantity:     match.Quantity,
			Rate:         match.Rate,
			Address:      extractAddress(match.Taker),
			ServerTime:   stamp,
			FeeRateBase:  match.FeeRateBase,
			FeeRateQuote: match.FeeRateQuote,
			Side:         uint8(order.Maker),
		}, &msgjson.Match{
			OrderID:      idToBytes(match.Taker.ID()),
			MatchID:      idToBytes(match.ID()),
			Quantity:     match.Quantity,
			Rate:         match.Rate,
			Address:      extractAddress(match.Maker),
			ServerTime:   stamp,
			FeeRateBase:  match.FeeRateBase,
			FeeRateQuote: match.FeeRateQuote,
			Side:         uint8(order.Taker),
		}
}

// readMatches translates a slice of raw matches from the market manager into
// a slice of matchTrackers.
func readMatches(matchSets []*order.MatchSet, feeRates map[uint32]uint64) []*matchTracker {
	// The initial capacity guess here is a minimum, but will avoid a few
	// reallocs.
	nowMs := unixMsNow()
	matches := make([]*matchTracker, 0, len(matchSets))
	for _, matchSet := range matchSets {
		for _, match := range matchSet.Matches( /* consider taking feeRates */ ) {
			maker := match.Maker
			base, quote := maker.BaseAsset, maker.QuoteAsset
			var makerSwapAsset, takerSwapAsset uint32
			if maker.Sell {
				makerSwapAsset = base
				takerSwapAsset = quote
			} else {
				makerSwapAsset = quote
				takerSwapAsset = base
			}

			match.FeeRateBase = feeRates[base]
			match.FeeRateQuote = feeRates[quote]

			matches = append(matches, &matchTracker{
				Match:     match,
				time:      nowMs,
				matchTime: match.Epoch.End(),
				makerStatus: &swapStatus{
					swapAsset:   makerSwapAsset,
					redeemAsset: takerSwapAsset,
				},
				takerStatus: &swapStatus{
					swapAsset:   takerSwapAsset,
					redeemAsset: makerSwapAsset,
				},
			})
		}
	}
	return matches
}

// Negotiate takes ownership of the matches and begins swap negotiation. For
// reliable identification of completed orders when redeem acks are received and
// processed by processAck, BeginMatchAndNegotiate should be called prior to
// matching and order status/amount updates, and EndMatchAndNegotiate should be
// called after Negotiate. This locking sequence allows for orders that may
// already be involved in active swaps to remain unmodified by the
// Matcher/Market until new matches are recorded by the Swapper in Negotiate. If
// this is not done, it is possible that an order may be flagged as completed if
// a swap A completes after Matching and creation of swap B but before Negotiate
// has a chance to record the new swap.
func (s *Swapper) Negotiate(matchSets []*order.MatchSet, finalSwap map[order.OrderID]bool) {
	// If the Swapper is stopping, the Markets should also be stopping, but
	// block this just in case.
	s.handlerMtx.RLock()
	defer s.handlerMtx.RUnlock()
	if s.stop {
		log.Errorf("Negotiate called on stopped swapper. Matches lost!")
		return
	}

	feeRates := make(map[uint32]uint64, 2) // should only be 2, but Negotiate might be called with matches with more than two assets in the future

	getFeeRate := func(assetID uint32) (unsupported bool) {
		if _, found := feeRates[assetID]; found {
			return
		}
		asset := s.coins[assetID]
		if asset == nil {
			return true
		}
		maxFeeRate := asset.Asset.MaxFeeRate
		feeRate, err := asset.Backend.FeeRate()
		if err != nil {
			feeRate = maxFeeRate
			log.Warnf("Unable to determining optimal fee rate for %q, using fallback of %d. Err: %v",
				asset.Symbol, feeRate, err)
		} else {
			log.Debugf("Optimal fee rate for %q: %d", asset.Symbol, feeRate)
			if feeRate > maxFeeRate {
				log.Warnf("Optimal fee rate %d > max fee rate %d, using max fee rate.",
					feeRate, maxFeeRate)
				feeRate = maxFeeRate
			}
		}
		feeRates[assetID] = feeRate
		return
	}

	// Lock trade order coins, and get current optimal fee rates. Also filter
	// out matches with unsupported assets, which should not happen if the
	// Market is behaving, but be defensive.
	supportedMatchSets := matchSets[:0]                    // same buffer, start empty
	swapOrders := make([]order.Order, 0, 2*len(matchSets)) // size guess, with the single maker case
	for _, match := range matchSets {
		// Do the fee rate requests first to verify asset support.
		if base := match.Taker.Base(); getFeeRate(base) {
			log.Errorf("Unsupported asset %d for order %v", base, match.Taker)
			continue
		}
		if quote := match.Taker.Quote(); getFeeRate(quote) {
			log.Errorf("Unsupported asset %d for order %v", quote, match.Taker)
			continue
		}

		supportedMatchSets = append(supportedMatchSets, match)

		if match.Taker.Type() == order.CancelOrderType {
			continue
		}

		swapOrders = append(swapOrders, match.Taker)
		for _, maker := range match.Makers {
			swapOrders = append(swapOrders, maker)
		}
	}
	matchSets = supportedMatchSets

	s.LockOrdersCoins(swapOrders)

	// Set up the matchTrackers, which includes a slice of Matches.
	matches := readMatches(matchSets, feeRates)

	// Record the matches. If any DB updates fail, no swaps proceed. We could
	// let the others proceed, but that could seem selective trickery to the
	// clients.
	for _, match := range matches {
		// Note that matches where the taker order is a cancel will be stored
		// with status MatchComplete, and without the maker or taker swap
		// addresses. The match will also be flagged as inactive since there is
		// no associated swap negotiation.

		// TODO: Initially store cancel matches lacking ack sigs as active, only
		// flagging as inactive when both maker and taker match ack sigs have
		// been received. The client will need a mechanism to provide the ack,
		// perhaps having the server resend missing match ack requests on client
		// connect.
		if err := s.storage.InsertMatch(match.Match); err != nil {
			log.Errorf("InsertMatch (match id=%v) failed: %v", match.ID(), err)
			// TODO: notify clients (notification or response to what?)
			// abortAll()
			return
		}
	}

	userMatches := make(map[account.AccountID][]*messageAcker)
	// addUserMatch signs a match notification message, and add the data
	// required to process the acknowledgment to the userMatches map.
	addUserMatch := func(acker *messageAcker) {
		s.authMgr.Sign(acker.params)
		userMatches[acker.user] = append(userMatches[acker.user], acker)
	}

	// Setting length to max possible, which is over-allocating by the number of
	// cancels.
	toMonitor := make([]*matchTracker, 0, len(matches))
	var canceled []*order.LimitOrder
	for _, match := range matches {
		if match.Taker.Type() == order.CancelOrderType {
			// The canceled orders must be flagged after new swaps are counted.
			canceled = append(canceled, match.Maker)

			// If this is a cancellation, there is nothing to track. Just cancel
			// the target order by removing it from the DB. It is already
			// removed from book by the Market.
			err := s.storage.CancelOrder(match.Maker)
			if err != nil {
				log.Errorf("Failed to cancel order %v", match.Maker)
				// If the DB update failed, the target order status was not
				// updated, but removed from the in-memory book. This is
				// potentially a critical failure since the dex will restore the
				// book from the DB. TODO: Notify clients.
				return
			}
		} else {
			toMonitor = append(toMonitor, match)
			s.orders.incActiveSwapCount(match.Maker, finalSwap[match.Maker.ID()])
			s.orders.incActiveSwapCount(match.Taker, finalSwap[match.Taker.ID()])
		}

		// Create an acker for maker and taker, sharing the same matchTracker.
		makerMsg, takerMsg := matchNotifications(match) // msgjson.Match for each party
		addUserMatch(&messageAcker{
			user:    match.Maker.User(),
			match:   match,
			params:  makerMsg,
			isMaker: true,
			// isAudit: false,
		})
		addUserMatch(&messageAcker{
			user:    match.Taker.User(),
			match:   match,
			params:  takerMsg,
			isMaker: false,
			// isAudit: false,
		})
	}

	// Flag the canceled orders as failed and off-book if they are involved in
	// active swaps from this or previous epochs.
	for _, lo := range canceled {
		s.orders.canceled(lo)
	}

	// Add the matches to the matches/userMatches maps.
	s.matchMtx.Lock()
	for _, match := range toMonitor {
		s.addMatch(match)
	}
	s.matchMtx.Unlock()

	// Send the user match notifications.
	for user, matches := range userMatches {
		// msgs is a slice of msgjson.Match created by newMatchAckers
		// (matchNotifications) for all makers and takers.
		msgs := make([]msgjson.Signable, 0, len(matches))
		for _, m := range matches {
			msgs = append(msgs, m.params)
		}

		// Solicit match acknowledgments. Each Match is signed in addUserMatch.
		req, err := msgjson.NewRequest(comms.NextID(), msgjson.MatchRoute, msgs)
		if err != nil {
			log.Errorf("error creating match notification request: %v", err)
			// TODO: prevent user penalty
			continue
		}

		// Copy the loop variables for capture by the match acknowledgement
		// response handler.
		u, m := user, matches
		log.Debugf("Negotiate: sending 'match' ack request to user %v for %d matches",
			u, len(m))

		// Send the request.
		s.authMgr.Request(u, req, func(_ comms.Link, resp *msgjson.Message) {
			s.processMatchAcks(u, resp, m)
		})
	}
}

func idToBytes(id [order.OrderIDSize]byte) []byte {
	return id[:]
}
