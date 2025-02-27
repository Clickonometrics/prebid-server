package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prebid/prebid-server/adapters"
	"github.com/prebid/prebid-server/config"
	"github.com/prebid/prebid-server/currency"
	"github.com/prebid/prebid-server/errortypes"
	"github.com/prebid/prebid-server/experiment/adscert"
	"github.com/prebid/prebid-server/firstpartydata"
	"github.com/prebid/prebid-server/gdpr"
	"github.com/prebid/prebid-server/hooks/hookexecution"
	"github.com/prebid/prebid-server/metrics"
	"github.com/prebid/prebid-server/openrtb_ext"
	"github.com/prebid/prebid-server/prebid_cache_client"
	"github.com/prebid/prebid-server/stored_requests"
	"github.com/prebid/prebid-server/stored_responses"
	"github.com/prebid/prebid-server/usersync"
	"github.com/prebid/prebid-server/util/maputil"

	"github.com/buger/jsonparser"
	"github.com/gofrs/uuid"
	"github.com/golang/glog"
	"github.com/prebid/openrtb/v17/openrtb2"
	"github.com/prebid/openrtb/v17/openrtb3"
)

type extCacheInstructions struct {
	cacheBids, cacheVAST, returnCreative bool
}

// Exchange runs Auctions. Implementations must be threadsafe, and will be shared across many goroutines.
type Exchange interface {
	// HoldAuction executes an OpenRTB v2.5 Auction.
	HoldAuction(ctx context.Context, r AuctionRequest, debugLog *DebugLog) (*openrtb2.BidResponse, error)
}

// IdFetcher can find the user's ID for a specific Bidder.
type IdFetcher interface {
	GetUID(key string) (uid string, exists bool, notExpired bool)
	HasAnyLiveSyncs() bool
}

type exchange struct {
	adapterMap        map[openrtb_ext.BidderName]AdaptedBidder
	bidderInfo        config.BidderInfos
	bidderToSyncerKey map[string]string
	me                metrics.MetricsEngine
	cache             prebid_cache_client.Client
	cacheTime         time.Duration
	gdprPermsBuilder  gdpr.PermissionsBuilder
	tcf2ConfigBuilder gdpr.TCF2ConfigBuilder
	currencyConverter *currency.RateConverter
	externalURL       string
	gdprDefaultValue  gdpr.Signal
	privacyConfig     config.Privacy
	categoriesFetcher stored_requests.CategoryFetcher
	bidIDGenerator    BidIDGenerator
	hostSChainNode    *openrtb2.SupplyChainNode
	adsCertSigner     adscert.Signer
	server            config.Server
}

// Container to pass out response ext data from the GetAllBids goroutines back into the main thread
type seatResponseExtra struct {
	ResponseTimeMillis int
	Errors             []openrtb_ext.ExtBidderMessage
	Warnings           []openrtb_ext.ExtBidderMessage
	// httpCalls is the list of debugging info. It should only be populated if the request.test == 1.
	// This will become response.ext.debug.httpcalls.{bidder} on the final Response.
	HttpCalls []*openrtb_ext.ExtHttpCall
}

type bidResponseWrapper struct {
	adapterSeatBids []*pbsOrtbSeatBid
	adapterExtra    *seatResponseExtra
	bidder          openrtb_ext.BidderName
}

type BidIDGenerator interface {
	New() (string, error)
	Enabled() bool
}

type bidIDGenerator struct {
	enabled bool
}

func (big *bidIDGenerator) Enabled() bool {
	return big.enabled
}

func (big *bidIDGenerator) New() (string, error) {
	rawUuid, err := uuid.NewV4()
	return rawUuid.String(), err
}

type deduplicateChanceGenerator interface {
	Generate() bool
}

type randomDeduplicateBidBooleanGenerator struct{}

func (randomDeduplicateBidBooleanGenerator) Generate() bool {
	return rand.Intn(100) < 50
}

func NewExchange(adapters map[openrtb_ext.BidderName]AdaptedBidder, cache prebid_cache_client.Client, cfg *config.Configuration, syncersByBidder map[string]usersync.Syncer, metricsEngine metrics.MetricsEngine, infos config.BidderInfos, gdprPermsBuilder gdpr.PermissionsBuilder, tcf2CfgBuilder gdpr.TCF2ConfigBuilder, currencyConverter *currency.RateConverter, categoriesFetcher stored_requests.CategoryFetcher, adsCertSigner adscert.Signer) Exchange {
	bidderToSyncerKey := map[string]string{}
	for bidder, syncer := range syncersByBidder {
		bidderToSyncerKey[bidder] = syncer.Key()
	}

	gdprDefaultValue := gdpr.SignalYes
	if cfg.GDPR.DefaultValue == "0" {
		gdprDefaultValue = gdpr.SignalNo
	}

	return &exchange{
		adapterMap:        adapters,
		bidderInfo:        infos,
		bidderToSyncerKey: bidderToSyncerKey,
		cache:             cache,
		cacheTime:         time.Duration(cfg.CacheURL.ExpectedTimeMillis) * time.Millisecond,
		categoriesFetcher: categoriesFetcher,
		currencyConverter: currencyConverter,
		externalURL:       cfg.ExternalURL,
		gdprPermsBuilder:  gdprPermsBuilder,
		tcf2ConfigBuilder: tcf2CfgBuilder,
		me:                metricsEngine,
		gdprDefaultValue:  gdprDefaultValue,
		privacyConfig: config.Privacy{
			CCPA: cfg.CCPA,
			GDPR: cfg.GDPR,
			LMT:  cfg.LMT,
		},
		bidIDGenerator: &bidIDGenerator{cfg.GenerateBidID},
		hostSChainNode: cfg.HostSChainNode,
		adsCertSigner:  adsCertSigner,
		server:         config.Server{ExternalUrl: cfg.ExternalURL, GvlID: cfg.GDPR.HostVendorID, DataCenter: cfg.DataCenter},
	}
}

type ImpExtInfo struct {
	EchoVideoAttrs bool
	StoredImp      []byte
	Passthrough    json.RawMessage
}

// AuctionRequest holds the bid request for the auction
// and all other information needed to process that request
type AuctionRequest struct {
	BidRequestWrapper          *openrtb_ext.RequestWrapper
	ResolvedBidRequest         json.RawMessage
	Account                    config.Account
	UserSyncs                  IdFetcher
	RequestType                metrics.RequestType
	StartTime                  time.Time
	Warnings                   []error
	GlobalPrivacyControlHeader string
	ImpExtInfoMap              map[string]ImpExtInfo

	// LegacyLabels is included here for temporary compatibility with cleanOpenRTBRequests
	// in HoldAuction until we get to factoring it away. Do not use for anything new.
	LegacyLabels   metrics.Labels
	FirstPartyData map[openrtb_ext.BidderName]*firstpartydata.ResolvedFirstPartyData
	// map of imp id to stored response
	StoredAuctionResponses stored_responses.ImpsWithBidResponses
	// map of imp id to bidder to stored response
	StoredBidResponses    stored_responses.ImpBidderStoredResp
	BidderImpReplaceImpID stored_responses.BidderImpReplaceImpID
	PubID                 string
	HookExecutor          hookexecution.StageExecutor
}

// BidderRequest holds the bidder specific request and all other
// information needed to process that bidder request.
type BidderRequest struct {
	BidRequest            *openrtb2.BidRequest
	BidderName            openrtb_ext.BidderName
	BidderCoreName        openrtb_ext.BidderName
	BidderLabels          metrics.AdapterLabels
	BidderStoredResponses map[string]json.RawMessage
	ImpReplaceImpId       map[string]bool
}

func (e *exchange) HoldAuction(ctx context.Context, r AuctionRequest, debugLog *DebugLog) (*openrtb2.BidResponse, error) {
	reject := r.HookExecutor.ExecuteProcessedAuctionStage(r.BidRequestWrapper.BidRequest)
	if reject != nil {
		return nil, reject
	}

	var errs []error
	// rebuild/resync the request in the request wrapper.
	if err := r.BidRequestWrapper.RebuildRequest(); err != nil {
		return nil, err
	}
	requestExt, err := extractBidRequestExt(r.BidRequestWrapper.BidRequest)
	if err != nil {
		return nil, err
	}
	if !e.server.Empty() {
		requestExt.Prebid.Server = &openrtb_ext.ExtRequestPrebidServer{ExternalUrl: e.server.ExternalUrl, GvlID: e.server.GvlID, DataCenter: e.server.DataCenter}
	}

	cacheInstructions := getExtCacheInstructions(requestExt)
	targData := getExtTargetData(requestExt, &cacheInstructions)
	if targData != nil {
		_, targData.cacheHost, targData.cachePath = e.cache.GetExtCacheData()
	}
	responseDebugAllow, accountDebugAllow, debugLog := getDebugInfo(r.BidRequestWrapper.BidRequest, requestExt, r.Account.DebugAllow, debugLog)
	if responseDebugAllow {
		//save incoming request with stored requests (if applicable) to return in debug logs
		resolvedBidReq, err := json.Marshal(r.BidRequestWrapper.BidRequest)
		if err != nil {
			return nil, err
		}
		r.ResolvedBidRequest = resolvedBidReq
	}
	e.me.RecordDebugRequest(responseDebugAllow || accountDebugAllow, r.PubID)

	if r.RequestType == metrics.ReqTypeORTB2Web || r.RequestType == metrics.ReqTypeORTB2App {
		//Extract First party data for auction endpoint only
		resolvedFPD, fpdErrors := firstpartydata.ExtractFPDForBidders(r.BidRequestWrapper)
		if len(fpdErrors) > 0 {
			var errMessages []string
			for _, fpdError := range fpdErrors {
				errMessages = append(errMessages, fpdError.Error())
			}
			return nil, &errortypes.BadInput{
				Message: strings.Join(errMessages, ","),
			}
		}
		r.FirstPartyData = resolvedFPD
		if len(resolvedFPD) > 0 {
			// rebuild/resync the request in the request wrapper.
			// it needs to be here to update ReqWrapper and requestExt because req.ext was modified after FPD extraction
			if err := r.BidRequestWrapper.RebuildRequest(); err != nil {
				return nil, err
			}
			requestExt, err = extractBidRequestExt(r.BidRequestWrapper.BidRequest)
			if err != nil {
				return nil, err
			}
		}
	}

	bidAdjustmentFactors := getExtBidAdjustmentFactors(requestExt)

	recordImpMetrics(r.BidRequestWrapper.BidRequest, e.me)

	// Make our best guess if GDPR applies
	gdprDefaultValue := e.parseGDPRDefaultValue(r.BidRequestWrapper.BidRequest)

	// Slice of BidRequests, each a copy of the original cleaned to only contain bidder data for the named bidder
	bidderRequests, privacyLabels, errs := cleanOpenRTBRequests(ctx, r, requestExt, e.bidderToSyncerKey, e.me, gdprDefaultValue, e.privacyConfig, e.gdprPermsBuilder, e.tcf2ConfigBuilder, e.hostSChainNode)

	e.me.RecordRequestPrivacy(privacyLabels)

	if len(r.StoredAuctionResponses) > 0 || len(r.StoredBidResponses) > 0 {
		e.me.RecordStoredResponse(r.PubID)
	}

	// If we need to cache bids, then it will take some time to call prebid cache.
	// We should reduce the amount of time the bidders have, to compensate.
	auctionCtx, cancel := e.makeAuctionContext(ctx, cacheInstructions.cacheBids)
	defer cancel()

	// Get currency rates conversions for the auction
	conversions := e.getAuctionCurrencyRates(requestExt.Prebid.CurrencyConversions)

	var adapterBids map[openrtb_ext.BidderName]*pbsOrtbSeatBid
	var adapterExtra map[openrtb_ext.BidderName]*seatResponseExtra
	var anyBidsReturned bool

	// List of bidders we have requests for.
	var liveAdapters []openrtb_ext.BidderName

	if len(r.StoredAuctionResponses) > 0 {
		adapterBids, liveAdapters, err = buildStoredAuctionResponse(r.StoredAuctionResponses)
		if err != nil {
			return nil, err
		}
		anyBidsReturned = true

	} else {
		// List of bidders we have requests for.
		liveAdapters = listBiddersWithRequests(bidderRequests)

		//This will be used to validate bids
		var alternateBidderCodes openrtb_ext.ExtAlternateBidderCodes
		if requestExt != nil && requestExt.Prebid.AlternateBidderCodes != nil {
			alternateBidderCodes = *requestExt.Prebid.AlternateBidderCodes
		} else if r.Account.AlternateBidderCodes != nil {
			alternateBidderCodes = *r.Account.AlternateBidderCodes
		}

		adapterBids, adapterExtra, anyBidsReturned = e.getAllBids(auctionCtx, bidderRequests, bidAdjustmentFactors, conversions, accountDebugAllow, r.GlobalPrivacyControlHeader, debugLog.DebugOverride, alternateBidderCodes, requestExt.Prebid.Experiment)
	}

	var auc *auction
	var cacheErrs []error
	var bidResponseExt *openrtb_ext.ExtBidResponse
	if anyBidsReturned {

		var bidCategory map[string]string
		//If includebrandcategory is present in ext then CE feature is on.
		if requestExt.Prebid.Targeting != nil && requestExt.Prebid.Targeting.IncludeBrandCategory != nil {
			var rejections []string
			bidCategory, adapterBids, rejections, err = applyCategoryMapping(ctx, requestExt, adapterBids, e.categoriesFetcher, targData, &randomDeduplicateBidBooleanGenerator{})
			if err != nil {
				return nil, fmt.Errorf("Error in category mapping : %s", err.Error())
			}
			for _, message := range rejections {
				errs = append(errs, errors.New(message))
			}
		}

		if e.bidIDGenerator.Enabled() {
			for _, seatBid := range adapterBids {
				for _, pbsBid := range seatBid.bids {
					pbsBid.generatedBidID, err = e.bidIDGenerator.New()
					if err != nil {
						errs = append(errs, errors.New("Error generating bid.ext.prebid.bidid"))
					}
				}
			}
		}

		evTracking := getEventTracking(&requestExt.Prebid, r.StartTime, &r.Account, e.bidderInfo, e.externalURL)
		adapterBids = evTracking.modifyBidsForEvents(adapterBids)

		if targData != nil {
			// A non-nil auction is only needed if targeting is active. (It is used below this block to extract cache keys)
			auc = newAuction(adapterBids, len(r.BidRequestWrapper.Imp), targData.preferDeals)
			auc.setRoundedPrices(targData.priceGranularity)

			if requestExt.Prebid.SupportDeals {
				dealErrs := applyDealSupport(r.BidRequestWrapper.BidRequest, auc, bidCategory)
				errs = append(errs, dealErrs...)
			}

			bidResponseExt = e.makeExtBidResponse(adapterBids, adapterExtra, r, responseDebugAllow, requestExt.Prebid.Passthrough, errs)
			if debugLog.DebugEnabledOrOverridden {
				if bidRespExtBytes, err := json.Marshal(bidResponseExt); err == nil {
					debugLog.Data.Response = string(bidRespExtBytes)
				} else {
					debugLog.Data.Response = "Unable to marshal response ext for debugging"
					errs = append(errs, err)
				}
			}

			cacheErrs = auc.doCache(ctx, e.cache, targData, evTracking, r.BidRequestWrapper.BidRequest, 60, &r.Account.CacheTTL, bidCategory, debugLog)
			if len(cacheErrs) > 0 {
				errs = append(errs, cacheErrs...)
			}

			targData.setTargeting(auc, r.BidRequestWrapper.BidRequest.App != nil, bidCategory, r.Account.TruncateTargetAttribute)

		}
		bidResponseExt = e.makeExtBidResponse(adapterBids, adapterExtra, r, responseDebugAllow, requestExt.Prebid.Passthrough, errs)
	} else {
		bidResponseExt = e.makeExtBidResponse(adapterBids, adapterExtra, r, responseDebugAllow, requestExt.Prebid.Passthrough, errs)

		if debugLog.DebugEnabledOrOverridden {

			if bidRespExtBytes, err := json.Marshal(bidResponseExt); err == nil {
				debugLog.Data.Response = string(bidRespExtBytes)
			} else {
				debugLog.Data.Response = "Unable to marshal response ext for debugging"
				errs = append(errs, err)
			}
		}
	}

	if !accountDebugAllow && !debugLog.DebugOverride {
		accountDebugDisabledWarning := openrtb_ext.ExtBidderMessage{
			Code:    errortypes.AccountLevelDebugDisabledWarningCode,
			Message: "debug turned off for account",
		}
		bidResponseExt.Warnings[openrtb_ext.BidderReservedGeneral] = append(bidResponseExt.Warnings[openrtb_ext.BidderReservedGeneral], accountDebugDisabledWarning)
	}

	for _, warning := range r.Warnings {
		generalWarning := openrtb_ext.ExtBidderMessage{
			Code:    errortypes.ReadCode(warning),
			Message: warning.Error(),
		}
		bidResponseExt.Warnings[openrtb_ext.BidderReservedGeneral] = append(bidResponseExt.Warnings[openrtb_ext.BidderReservedGeneral], generalWarning)
	}

	// Build the response
	return e.buildBidResponse(ctx, liveAdapters, adapterBids, r.BidRequestWrapper.BidRequest, adapterExtra, auc, bidResponseExt, cacheInstructions.returnCreative, r.ImpExtInfoMap, errs)
}

func (e *exchange) parseGDPRDefaultValue(bidRequest *openrtb2.BidRequest) gdpr.Signal {
	gdprDefaultValue := e.gdprDefaultValue
	var geo *openrtb2.Geo = nil

	if bidRequest.User != nil && bidRequest.User.Geo != nil {
		geo = bidRequest.User.Geo
	} else if bidRequest.Device != nil && bidRequest.Device.Geo != nil {
		geo = bidRequest.Device.Geo
	}
	if geo != nil {
		// If we have a country set, and it is on the list, we assume GDPR applies if not set on the request.
		// Otherwise we assume it does not apply as long as it appears "valid" (is 3 characters long).
		if _, found := e.privacyConfig.GDPR.EEACountriesMap[strings.ToUpper(geo.Country)]; found {
			gdprDefaultValue = gdpr.SignalYes
		} else if len(geo.Country) == 3 {
			// The country field is formatted properly as a three character country code
			gdprDefaultValue = gdpr.SignalNo
		}
	}

	return gdprDefaultValue
}

func recordImpMetrics(bidRequest *openrtb2.BidRequest, metricsEngine metrics.MetricsEngine) {
	for _, impInRequest := range bidRequest.Imp {
		var impLabels metrics.ImpLabels = metrics.ImpLabels{
			BannerImps: impInRequest.Banner != nil,
			VideoImps:  impInRequest.Video != nil,
			AudioImps:  impInRequest.Audio != nil,
			NativeImps: impInRequest.Native != nil,
		}
		metricsEngine.RecordImps(impLabels)
	}
}

// applyDealSupport updates targeting keys with deal prefixes if minimum deal tier exceeded
func applyDealSupport(bidRequest *openrtb2.BidRequest, auc *auction, bidCategory map[string]string) []error {
	errs := []error{}
	impDealMap := getDealTiers(bidRequest)

	for impID, topBidsPerImp := range auc.winningBidsByBidder {
		impDeal := impDealMap[impID]
		for bidder, topBidPerBidder := range topBidsPerImp {
			if topBidPerBidder.dealPriority > 0 {
				if validateDealTier(impDeal[bidder]) {
					updateHbPbCatDur(topBidPerBidder, impDeal[bidder], bidCategory)
				} else {
					errs = append(errs, fmt.Errorf("dealTier configuration invalid for bidder '%s', imp ID '%s'", string(bidder), impID))
				}
			}
		}
	}

	return errs
}

// getDealTiers creates map of impression to bidder deal tier configuration
func getDealTiers(bidRequest *openrtb2.BidRequest) map[string]openrtb_ext.DealTierBidderMap {
	impDealMap := make(map[string]openrtb_ext.DealTierBidderMap)

	for _, imp := range bidRequest.Imp {
		dealTierBidderMap, err := openrtb_ext.ReadDealTiersFromImp(imp)
		if err != nil {
			continue
		}
		impDealMap[imp.ID] = dealTierBidderMap
	}

	return impDealMap
}

func validateDealTier(dealTier openrtb_ext.DealTier) bool {
	return len(dealTier.Prefix) > 0 && dealTier.MinDealTier > 0
}

func updateHbPbCatDur(bid *pbsOrtbBid, dealTier openrtb_ext.DealTier, bidCategory map[string]string) {
	if bid.dealPriority >= dealTier.MinDealTier {
		prefixTier := fmt.Sprintf("%s%d_", dealTier.Prefix, bid.dealPriority)
		bid.dealTierSatisfied = true

		if oldCatDur, ok := bidCategory[bid.bid.ID]; ok {
			oldCatDurSplit := strings.SplitAfterN(oldCatDur, "_", 2)
			oldCatDurSplit[0] = prefixTier

			newCatDur := strings.Join(oldCatDurSplit, "")
			bidCategory[bid.bid.ID] = newCatDur
		}
	}
}

func (e *exchange) makeAuctionContext(ctx context.Context, needsCache bool) (auctionCtx context.Context, cancel context.CancelFunc) {
	auctionCtx = ctx
	cancel = func() {}
	if needsCache {
		if deadline, ok := ctx.Deadline(); ok {
			auctionCtx, cancel = context.WithDeadline(ctx, deadline.Add(-e.cacheTime))
		}
	}
	return
}

// This piece sends all the requests to the bidder adapters and gathers the results.
func (e *exchange) getAllBids(
	ctx context.Context,
	bidderRequests []BidderRequest,
	bidAdjustments map[string]float64,
	conversions currency.Conversions,
	accountDebugAllowed bool,
	globalPrivacyControlHeader string,
	headerDebugAllowed bool,
	alternateBidderCodes openrtb_ext.ExtAlternateBidderCodes,
	experiment *openrtb_ext.Experiment) (
	map[openrtb_ext.BidderName]*pbsOrtbSeatBid,
	map[openrtb_ext.BidderName]*seatResponseExtra, bool) {
	// Set up pointers to the bid results
	adapterBids := make(map[openrtb_ext.BidderName]*pbsOrtbSeatBid, len(bidderRequests))
	adapterExtra := make(map[openrtb_ext.BidderName]*seatResponseExtra, len(bidderRequests))
	chBids := make(chan *bidResponseWrapper, len(bidderRequests))
	bidsFound := false

	for _, bidder := range bidderRequests {
		// Here we actually call the adapters and collect the bids.
		bidderRunner := e.recoverSafely(bidderRequests, func(bidderRequest BidderRequest, conversions currency.Conversions) {
			// Passing in aName so a doesn't change out from under the go routine
			if bidderRequest.BidderLabels.Adapter == "" {
				glog.Errorf("Exchange: bidlables for %s (%s) missing adapter string", bidderRequest.BidderName, bidderRequest.BidderCoreName)
				bidderRequest.BidderLabels.Adapter = bidderRequest.BidderCoreName
			}
			brw := new(bidResponseWrapper)
			brw.bidder = bidderRequest.BidderName
			// Defer basic metrics to insure we capture them after all the values have been set
			defer func() {
				e.me.RecordAdapterRequest(bidderRequest.BidderLabels)
			}()
			start := time.Now()

			reqInfo := adapters.NewExtraRequestInfo(conversions)
			reqInfo.PbsEntryPoint = bidderRequest.BidderLabels.RType
			reqInfo.GlobalPrivacyControlHeader = globalPrivacyControlHeader

			bidReqOptions := bidRequestOptions{
				accountDebugAllowed: accountDebugAllowed,
				headerDebugAllowed:  headerDebugAllowed,
				addCallSignHeader:   isAdsCertEnabled(experiment, e.bidderInfo[string(bidderRequest.BidderName)]),
				bidAdjustments:      bidAdjustments,
			}
			seatBids, err := e.adapterMap[bidderRequest.BidderCoreName].requestBid(ctx, bidderRequest, conversions, &reqInfo, e.adsCertSigner, bidReqOptions, alternateBidderCodes)

			// Add in time reporting
			elapsed := time.Since(start)
			brw.adapterSeatBids = seatBids
			// Structure to record extra tracking data generated during bidding
			ae := new(seatResponseExtra)
			ae.ResponseTimeMillis = int(elapsed / time.Millisecond)
			if len(seatBids) != 0 {
				ae.HttpCalls = seatBids[0].httpCalls
			}

			// Timing statistics
			e.me.RecordAdapterTime(bidderRequest.BidderLabels, time.Since(start))
			bidderRequest.BidderLabels.AdapterBids = bidsToMetric(brw.adapterSeatBids)
			bidderRequest.BidderLabels.AdapterErrors = errorsToMetric(err)
			// Append any bid validation errors to the error list
			ae.Errors = errsToBidderErrors(err)
			ae.Warnings = errsToBidderWarnings(err)
			brw.adapterExtra = ae
			for _, seatBid := range seatBids {
				if seatBid != nil {
					for _, bid := range seatBid.bids {
						var cpm = float64(bid.bid.Price * 1000)
						e.me.RecordAdapterPrice(bidderRequest.BidderLabels, cpm)
						e.me.RecordAdapterBidReceived(bidderRequest.BidderLabels, bid.bidType, bid.bid.AdM != "")
					}
				}
			}
			chBids <- brw
		}, chBids)
		go bidderRunner(bidder, conversions)
	}
	// Wait for the bidders to do their thing
	for i := 0; i < len(bidderRequests); i++ {
		brw := <-chBids

		//if bidder returned no bids back - remove bidder from further processing
		for _, seatBid := range brw.adapterSeatBids {
			if seatBid != nil && len(seatBid.bids) != 0 {
				if _, ok := adapterBids[openrtb_ext.BidderName(seatBid.seat)]; ok {
					adapterBids[openrtb_ext.BidderName(seatBid.seat)].bids = append(adapterBids[openrtb_ext.BidderName(seatBid.seat)].bids, seatBid.bids...)
				} else {
					adapterBids[openrtb_ext.BidderName(seatBid.seat)] = seatBid
				}
			}
		}
		//but we need to add all bidders data to adapterExtra to have metrics and other metadata
		adapterExtra[brw.bidder] = brw.adapterExtra

		if !bidsFound && adapterBids[brw.bidder] != nil && len(adapterBids[brw.bidder].bids) > 0 {
			bidsFound = true
		}
	}

	return adapterBids, adapterExtra, bidsFound
}

func (e *exchange) recoverSafely(bidderRequests []BidderRequest,
	inner func(BidderRequest, currency.Conversions),
	chBids chan *bidResponseWrapper) func(BidderRequest, currency.Conversions) {
	return func(bidderRequest BidderRequest, conversions currency.Conversions) {
		defer func() {
			if r := recover(); r != nil {

				allBidders := ""
				sb := strings.Builder{}
				for _, bidder := range bidderRequests {
					sb.WriteString(bidder.BidderName.String())
					sb.WriteString(",")
				}
				if sb.Len() > 0 {
					allBidders = sb.String()[:sb.Len()-1]
				}

				glog.Errorf("OpenRTB auction recovered panic from Bidder %s: %v. "+
					"Account id: %s, All Bidders: %s, Stack trace is: %v",
					bidderRequest.BidderCoreName, r, bidderRequest.BidderLabels.PubID, allBidders, string(debug.Stack()))
				e.me.RecordAdapterPanic(bidderRequest.BidderLabels)
				// Let the master request know that there is no data here
				brw := new(bidResponseWrapper)
				brw.adapterExtra = new(seatResponseExtra)
				chBids <- brw
			}
		}()
		inner(bidderRequest, conversions)
	}
}

func bidsToMetric(seatBids []*pbsOrtbSeatBid) metrics.AdapterBid {
	for _, seatBid := range seatBids {
		if seatBid != nil && len(seatBid.bids) != 0 {
			return metrics.AdapterBidPresent
		}
	}
	return metrics.AdapterBidNone
}

func errorsToMetric(errs []error) map[metrics.AdapterError]struct{} {
	if len(errs) == 0 {
		return nil
	}
	ret := make(map[metrics.AdapterError]struct{}, len(errs))
	var s struct{}
	for _, err := range errs {
		switch errortypes.ReadCode(err) {
		case errortypes.TimeoutErrorCode:
			ret[metrics.AdapterErrorTimeout] = s
		case errortypes.BadInputErrorCode:
			ret[metrics.AdapterErrorBadInput] = s
		case errortypes.BadServerResponseErrorCode:
			ret[metrics.AdapterErrorBadServerResponse] = s
		case errortypes.FailedToRequestBidsErrorCode:
			ret[metrics.AdapterErrorFailedToRequestBids] = s
		case errortypes.AlternateBidderCodeWarningCode:
			ret[metrics.AdapterErrorValidation] = s
		default:
			ret[metrics.AdapterErrorUnknown] = s
		}
	}
	return ret
}

func errsToBidderErrors(errs []error) []openrtb_ext.ExtBidderMessage {
	sErr := make([]openrtb_ext.ExtBidderMessage, 0)
	for _, err := range errortypes.FatalOnly(errs) {
		newErr := openrtb_ext.ExtBidderMessage{
			Code:    errortypes.ReadCode(err),
			Message: err.Error(),
		}
		sErr = append(sErr, newErr)
	}

	return sErr
}

func errsToBidderWarnings(errs []error) []openrtb_ext.ExtBidderMessage {
	sWarn := make([]openrtb_ext.ExtBidderMessage, 0)
	for _, warn := range errortypes.WarningOnly(errs) {
		newErr := openrtb_ext.ExtBidderMessage{
			Code:    errortypes.ReadCode(warn),
			Message: warn.Error(),
		}
		sWarn = append(sWarn, newErr)
	}
	return sWarn
}

// This piece takes all the bids supplied by the adapters and crafts an openRTB response to send back to the requester
func (e *exchange) buildBidResponse(ctx context.Context, liveAdapters []openrtb_ext.BidderName, adapterSeatBids map[openrtb_ext.BidderName]*pbsOrtbSeatBid, bidRequest *openrtb2.BidRequest, adapterExtra map[openrtb_ext.BidderName]*seatResponseExtra, auc *auction, bidResponseExt *openrtb_ext.ExtBidResponse, returnCreative bool, impExtInfoMap map[string]ImpExtInfo, errList []error) (*openrtb2.BidResponse, error) {
	bidResponse := new(openrtb2.BidResponse)
	var err error

	bidResponse.ID = bidRequest.ID
	if len(liveAdapters) == 0 {
		// signal "Invalid Request" if no valid bidders.
		bidResponse.NBR = openrtb3.NoBidInvalidRequest.Ptr()
	}

	// Create the SeatBids. We use a zero sized slice so that we can append non-zero seat bids, and not include seatBid
	// objects for seatBids without any bids. Preallocate the max possible size to avoid reallocating the array as we go.
	seatBids := make([]openrtb2.SeatBid, 0, len(liveAdapters))
	for a, adapterSeatBids := range adapterSeatBids {
		//while processing every single bib, do we need to handle categories here?
		if adapterSeatBids != nil && len(adapterSeatBids.bids) > 0 {
			sb := e.makeSeatBid(adapterSeatBids, a, adapterExtra, auc, returnCreative, impExtInfoMap)
			seatBids = append(seatBids, *sb)
			bidResponse.Cur = adapterSeatBids.currency
		}
	}

	bidResponse.SeatBid = seatBids

	bidResponse.Ext, err = encodeBidResponseExt(bidResponseExt)

	return bidResponse, err
}

func encodeBidResponseExt(bidResponseExt *openrtb_ext.ExtBidResponse) ([]byte, error) {
	buffer := &bytes.Buffer{}
	enc := json.NewEncoder(buffer)

	enc.SetEscapeHTML(false)
	err := enc.Encode(bidResponseExt)

	return buffer.Bytes(), err
}

func applyCategoryMapping(ctx context.Context, requestExt *openrtb_ext.ExtRequest, seatBids map[openrtb_ext.BidderName]*pbsOrtbSeatBid, categoriesFetcher stored_requests.CategoryFetcher, targData *targetData, booleanGenerator deduplicateChanceGenerator) (map[string]string, map[openrtb_ext.BidderName]*pbsOrtbSeatBid, []string, error) {
	res := make(map[string]string)

	type bidDedupe struct {
		bidderName openrtb_ext.BidderName
		bidIndex   int
		bidID      string
		bidPrice   string
	}

	dedupe := make(map[string]bidDedupe)

	// applyCategoryMapping doesn't get called unless
	// requestExt.Prebid.Targeting != nil && requestExt.Prebid.Targeting.IncludeBrandCategory != nil
	brandCatExt := requestExt.Prebid.Targeting.IncludeBrandCategory

	//If ext.prebid.targeting.includebrandcategory is present in ext then competitive exclusion feature is on.
	var includeBrandCategory = brandCatExt != nil //if not present - category will no be appended
	appendBidderNames := requestExt.Prebid.Targeting.AppendBidderNames

	var primaryAdServer string
	var publisher string
	var err error
	var rejections []string
	var translateCategories = true

	if includeBrandCategory && brandCatExt.WithCategory {
		if brandCatExt.TranslateCategories != nil {
			translateCategories = *brandCatExt.TranslateCategories
		}
		//if translateCategories is set to false, ignore checking primaryAdServer and publisher
		if translateCategories {
			//if ext.prebid.targeting.includebrandcategory present but primaryadserver/publisher not present then error out the request right away.
			primaryAdServer, err = getPrimaryAdServer(brandCatExt.PrimaryAdServer) //1-Freewheel 2-DFP
			if err != nil {
				return res, seatBids, rejections, err
			}
			publisher = brandCatExt.Publisher
		}
	}

	seatBidsToRemove := make([]openrtb_ext.BidderName, 0)

	for bidderName, seatBid := range seatBids {
		bidsToRemove := make([]int, 0)
		for bidInd := range seatBid.bids {
			bid := seatBid.bids[bidInd]
			bidID := bid.bid.ID
			var duration int
			var category string
			var pb string

			if bid.bidVideo != nil {
				duration = bid.bidVideo.Duration
				category = bid.bidVideo.PrimaryCategory
			}
			if brandCatExt.WithCategory && category == "" {
				bidIabCat := bid.bid.Cat
				if len(bidIabCat) != 1 {
					//TODO: add metrics
					//on receiving bids from adapters if no unique IAB category is returned  or if no ad server category is returned discard the bid
					bidsToRemove = append(bidsToRemove, bidInd)
					rejections = updateRejections(rejections, bidID, "Bid did not contain a category")
					continue
				}
				if translateCategories {
					//if unique IAB category is present then translate it to the adserver category based on mapping file
					category, err = categoriesFetcher.FetchCategories(ctx, primaryAdServer, publisher, bidIabCat[0])
					if err != nil || category == "" {
						//TODO: add metrics
						//if mapping required but no mapping file is found then discard the bid
						bidsToRemove = append(bidsToRemove, bidInd)
						reason := fmt.Sprintf("Category mapping file for primary ad server: '%s', publisher: '%s' not found", primaryAdServer, publisher)
						rejections = updateRejections(rejections, bidID, reason)
						continue
					}
				} else {
					//category translation is disabled, continue with IAB category
					category = bidIabCat[0]
				}
			}

			// TODO: consider should we remove bids with zero duration here?

			pb = GetPriceBucket(bid.bid.Price, targData.priceGranularity)

			newDur := duration
			if len(requestExt.Prebid.Targeting.DurationRangeSec) > 0 {
				durationRange := requestExt.Prebid.Targeting.DurationRangeSec
				sort.Ints(durationRange)
				//if the bid is above the range of the listed durations (and outside the buffer), reject the bid
				if duration > durationRange[len(durationRange)-1] {
					bidsToRemove = append(bidsToRemove, bidInd)
					rejections = updateRejections(rejections, bidID, "Bid duration exceeds maximum allowed")
					continue
				}
				for _, dur := range durationRange {
					if duration <= dur {
						newDur = dur
						break
					}
				}
			}

			var categoryDuration string
			var dupeKey string
			if brandCatExt.WithCategory {
				categoryDuration = fmt.Sprintf("%s_%s_%ds", pb, category, newDur)
				dupeKey = category
			} else {
				categoryDuration = fmt.Sprintf("%s_%ds", pb, newDur)
				dupeKey = categoryDuration
			}

			if appendBidderNames {
				categoryDuration = fmt.Sprintf("%s_%s", categoryDuration, bidderName.String())
			}

			if dupe, ok := dedupe[dupeKey]; ok {

				dupeBidPrice, err := strconv.ParseFloat(dupe.bidPrice, 64)
				if err != nil {
					dupeBidPrice = 0
				}
				currBidPrice, err := strconv.ParseFloat(pb, 64)
				if err != nil {
					currBidPrice = 0
				}
				if dupeBidPrice == currBidPrice {
					if booleanGenerator.Generate() {
						dupeBidPrice = -1
					} else {
						currBidPrice = -1
					}
				}

				if dupeBidPrice < currBidPrice {
					if dupe.bidderName == bidderName {
						// An older bid from the current bidder
						bidsToRemove = append(bidsToRemove, dupe.bidIndex)
						rejections = updateRejections(rejections, dupe.bidID, "Bid was deduplicated")
					} else {
						// An older bid from a different seatBid we've already finished with
						oldSeatBid := (seatBids)[dupe.bidderName]
						rejections = updateRejections(rejections, dupe.bidID, "Bid was deduplicated")
						if len(oldSeatBid.bids) == 1 {
							seatBidsToRemove = append(seatBidsToRemove, dupe.bidderName)
						} else {
							// This is a very rare, but still possible case where bid needs to be removed from already processed bidder
							// This happens when current processing bidder has a bid that has same deduplication key as a bid from already processed bidder
							// and already processed bid was selected to be removed
							// See example of input data in unit test `TestCategoryMappingTwoBiddersManyBidsEachNoCategorySamePrice`
							// Need to remove bid by name, not index in this case
							removeBidById(oldSeatBid, dupe.bidID)
						}
					}
					delete(res, dupe.bidID)
				} else {
					// Remove this bid
					bidsToRemove = append(bidsToRemove, bidInd)
					rejections = updateRejections(rejections, bidID, "Bid was deduplicated")
					continue
				}
			}
			res[bidID] = categoryDuration
			dedupe[dupeKey] = bidDedupe{bidderName: bidderName, bidIndex: bidInd, bidID: bidID, bidPrice: pb}
		}

		if len(bidsToRemove) > 0 {
			sort.Ints(bidsToRemove)
			if len(bidsToRemove) == len(seatBid.bids) {
				//if all bids are invalid - remove entire seat bid
				seatBidsToRemove = append(seatBidsToRemove, bidderName)
			} else {
				bids := seatBid.bids
				for i := len(bidsToRemove) - 1; i >= 0; i-- {
					remInd := bidsToRemove[i]
					bids = append(bids[:remInd], bids[remInd+1:]...)
				}
				seatBid.bids = bids
			}
		}

	}
	for _, seatBidInd := range seatBidsToRemove {
		seatBids[seatBidInd].bids = nil
	}

	return res, seatBids, rejections, nil
}

func removeBidById(seatBid *pbsOrtbSeatBid, bidID string) {
	//Find index of bid to remove
	dupeBidIndex := -1
	for i, bid := range seatBid.bids {
		if bid.bid.ID == bidID {
			dupeBidIndex = i
			break
		}
	}
	if dupeBidIndex != -1 {
		if dupeBidIndex < len(seatBid.bids)-1 {
			seatBid.bids = append(seatBid.bids[:dupeBidIndex], seatBid.bids[dupeBidIndex+1:]...)
		} else if dupeBidIndex == len(seatBid.bids)-1 {
			seatBid.bids = seatBid.bids[:len(seatBid.bids)-1]
		}
	}
}

func updateRejections(rejections []string, bidID string, reason string) []string {
	message := fmt.Sprintf("bid rejected [bid ID: %s] reason: %s", bidID, reason)
	return append(rejections, message)
}

func getPrimaryAdServer(adServerId int) (string, error) {
	switch adServerId {
	case 1:
		return "freewheel", nil
	case 2:
		return "dfp", nil
	default:
		return "", fmt.Errorf("Primary ad server %d not recognized", adServerId)
	}
}

// Extract all the data from the SeatBids and build the ExtBidResponse
func (e *exchange) makeExtBidResponse(adapterBids map[openrtb_ext.BidderName]*pbsOrtbSeatBid, adapterExtra map[openrtb_ext.BidderName]*seatResponseExtra, r AuctionRequest, debugInfo bool, passthrough json.RawMessage, errList []error) *openrtb_ext.ExtBidResponse {
	bidResponseExt := &openrtb_ext.ExtBidResponse{
		Errors:               make(map[openrtb_ext.BidderName][]openrtb_ext.ExtBidderMessage, len(adapterBids)),
		Warnings:             make(map[openrtb_ext.BidderName][]openrtb_ext.ExtBidderMessage, len(adapterBids)),
		ResponseTimeMillis:   make(map[openrtb_ext.BidderName]int, len(adapterBids)),
		RequestTimeoutMillis: r.BidRequestWrapper.BidRequest.TMax,
	}
	if debugInfo {
		bidResponseExt.Debug = &openrtb_ext.ExtResponseDebug{
			HttpCalls:       make(map[openrtb_ext.BidderName][]*openrtb_ext.ExtHttpCall),
			ResolvedRequest: r.ResolvedBidRequest,
		}
	}
	if !r.StartTime.IsZero() {
		// auctiontimestamp is the only response.ext.prebid attribute we may emit
		bidResponseExt.Prebid = &openrtb_ext.ExtResponsePrebid{
			AuctionTimestamp: r.StartTime.UnixNano() / 1e+6,
		}
	}

	if passthrough != nil {
		if bidResponseExt.Prebid == nil {
			bidResponseExt.Prebid = &openrtb_ext.ExtResponsePrebid{}
		}
		bidResponseExt.Prebid.Passthrough = passthrough
	}

	for bidderName, responseExtra := range adapterExtra {

		if debugInfo && len(responseExtra.HttpCalls) > 0 {
			bidResponseExt.Debug.HttpCalls[bidderName] = responseExtra.HttpCalls
		}
		if len(responseExtra.Warnings) > 0 {
			bidResponseExt.Warnings[bidderName] = responseExtra.Warnings
		}
		// Only make an entry for bidder errors if the bidder reported any.
		if len(responseExtra.Errors) > 0 {
			bidResponseExt.Errors[bidderName] = responseExtra.Errors
		}
		if len(errList) > 0 {
			bidResponseExt.Errors[openrtb_ext.PrebidExtKey] = errsToBidderErrors(errList)
		}
		bidResponseExt.ResponseTimeMillis[bidderName] = responseExtra.ResponseTimeMillis
		// Defering the filling of bidResponseExt.Usersync[bidderName] until later

	}
	return bidResponseExt
}

// Return an openrtb seatBid for a bidder
// BuildBidResponse is responsible for ensuring nil bid seatbids are not included
func (e *exchange) makeSeatBid(adapterBid *pbsOrtbSeatBid, adapter openrtb_ext.BidderName, adapterExtra map[openrtb_ext.BidderName]*seatResponseExtra, auc *auction, returnCreative bool, impExtInfoMap map[string]ImpExtInfo) *openrtb2.SeatBid {
	seatBid := &openrtb2.SeatBid{
		Seat:  adapter.String(),
		Group: 0, // Prebid cannot support roadblocking
	}

	var errList []error
	seatBid.Bid, errList = e.makeBid(adapterBid.bids, auc, returnCreative, impExtInfoMap)
	if len(errList) > 0 {
		adapterExtra[adapter].Errors = append(adapterExtra[adapter].Errors, errsToBidderErrors(errList)...)
	}

	return seatBid
}

func (e *exchange) makeBid(bids []*pbsOrtbBid, auc *auction, returnCreative bool, impExtInfoMap map[string]ImpExtInfo) ([]openrtb2.Bid, []error) {
	result := make([]openrtb2.Bid, 0, len(bids))
	errs := make([]error, 0, 1)

	for _, bid := range bids {
		bidExtPrebid := &openrtb_ext.ExtBidPrebid{
			DealPriority:      bid.dealPriority,
			DealTierSatisfied: bid.dealTierSatisfied,
			Events:            bid.bidEvents,
			Targeting:         bid.bidTargets,
			Type:              bid.bidType,
			Meta:              bid.bidMeta,
			Video:             bid.bidVideo,
			BidId:             bid.generatedBidID,
		}

		if cacheInfo, found := e.getBidCacheInfo(bid, auc); found {
			bidExtPrebid.Cache = &openrtb_ext.ExtBidPrebidCache{
				Bids: &cacheInfo,
			}
		}

		if bidExtJSON, err := makeBidExtJSON(bid.bid.Ext, bidExtPrebid, impExtInfoMap, bid.bid.ImpID, bid.originalBidCPM, bid.originalBidCur); err != nil {
			errs = append(errs, err)
		} else {
			result = append(result, *bid.bid)
			resultBid := &result[len(result)-1]
			resultBid.Ext = bidExtJSON
			if !returnCreative {
				resultBid.AdM = ""
			}
		}
	}
	return result, errs
}

func makeBidExtJSON(ext json.RawMessage, prebid *openrtb_ext.ExtBidPrebid, impExtInfoMap map[string]ImpExtInfo, impId string, originalBidCpm float64, originalBidCur string) (json.RawMessage, error) {
	var extMap map[string]interface{}

	if len(ext) != 0 {
		if err := json.Unmarshal(ext, &extMap); err != nil {
			return nil, err
		}
	} else {
		extMap = make(map[string]interface{})
	}

	//ext.origbidcpm
	if originalBidCpm >= 0 {
		extMap[openrtb_ext.OriginalBidCpmKey] = originalBidCpm
	}

	//ext.origbidcur
	if originalBidCur != "" {
		extMap[openrtb_ext.OriginalBidCurKey] = originalBidCur
	}

	// ext.prebid
	if prebid.Meta == nil && maputil.HasElement(extMap, "prebid", "meta") {
		metaContainer := struct {
			Prebid struct {
				Meta openrtb_ext.ExtBidPrebidMeta `json:"meta"`
			} `json:"prebid"`
		}{}
		if err := json.Unmarshal(ext, &metaContainer); err != nil {
			return nil, fmt.Errorf("error validaing response from server, %s", err)
		}
		prebid.Meta = &metaContainer.Prebid.Meta
	}

	// ext.prebid.storedrequestattributes and ext.prebid.passthrough
	if impExtInfo, ok := impExtInfoMap[impId]; ok {
		prebid.Passthrough = impExtInfoMap[impId].Passthrough
		if impExtInfo.EchoVideoAttrs {
			videoData, _, _, err := jsonparser.Get(impExtInfo.StoredImp, "video")
			if err != nil && err != jsonparser.KeyPathNotFoundError {
				return nil, err
			}
			//handler for case where EchoVideoAttrs is true, but video data is not found
			if len(videoData) > 0 {
				extMap[openrtb_ext.StoredRequestAttributes] = json.RawMessage(videoData)
			}
		}
	}
	extMap[openrtb_ext.PrebidExtKey] = prebid
	return json.Marshal(extMap)
}

// If bid got cached inside `(a *auction) doCache(ctx context.Context, cache prebid_cache_client.Client, targData *targetData, bidRequest *openrtb2.BidRequest, ttlBuffer int64, defaultTTLs *config.DefaultTTLs, bidCategory map[string]string)`,
// a UUID should be found inside `a.cacheIds` or `a.vastCacheIds`. This function returns the UUID along with the internal cache URL
func (e *exchange) getBidCacheInfo(bid *pbsOrtbBid, auction *auction) (cacheInfo openrtb_ext.ExtBidPrebidCacheBids, found bool) {
	uuid, found := findCacheID(bid, auction)

	if found {
		cacheInfo.CacheId = uuid
		cacheInfo.Url = buildCacheURL(e.cache, uuid)
	}

	return
}

func (e *exchange) getAuctionCurrencyRates(requestRates *openrtb_ext.ExtRequestCurrency) currency.Conversions {
	if requestRates == nil {
		// No bidRequest.ext.currency field was found, use PBS rates as usual
		return e.currencyConverter.Rates()
	}

	// If bidRequest.ext.currency.usepbsrates is nil, we understand its value as true. It will be false
	// only if it's explicitly set to false
	usePbsRates := requestRates.UsePBSRates == nil || *requestRates.UsePBSRates

	if !usePbsRates {
		// At this point, we can safely assume the ConversionRates map is not empty because
		// validateCustomRates(bidReqCurrencyRates *openrtb_ext.ExtRequestCurrency) would have
		// thrown an error under such conditions.
		return currency.NewRates(requestRates.ConversionRates)
	}

	// Both PBS and custom rates can be used, check if ConversionRates is not empty
	if len(requestRates.ConversionRates) == 0 {
		// Custom rates map is empty, use PBS rates only
		return e.currencyConverter.Rates()
	}

	// Return an AggregateConversions object that includes both custom and PBS currency rates but will
	// prioritize custom rates over PBS rates whenever a currency rate is found in both
	return currency.NewAggregateConversions(currency.NewRates(requestRates.ConversionRates), e.currencyConverter.Rates())
}

func findCacheID(bid *pbsOrtbBid, auction *auction) (string, bool) {
	if bid != nil && bid.bid != nil && auction != nil {
		if id, found := auction.cacheIds[bid.bid]; found {
			return id, true
		}

		if id, found := auction.vastCacheIds[bid.bid]; found {
			return id, true
		}
	}

	return "", false
}

func buildCacheURL(cache prebid_cache_client.Client, uuid string) string {
	scheme, host, path := cache.GetExtCacheData()

	if host == "" || path == "" {
		return ""
	}

	query := url.Values{"uuid": []string{uuid}}
	cacheURL := url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     path,
		RawQuery: query.Encode(),
	}
	cacheURL.Query()

	// URLs without a scheme will begin with //, in which case we
	// want to trim it off to keep compatbile with current behavior.
	return strings.TrimPrefix(cacheURL.String(), "//")
}

func listBiddersWithRequests(bidderRequests []BidderRequest) []openrtb_ext.BidderName {
	liveAdapters := make([]openrtb_ext.BidderName, len(bidderRequests))
	i := 0
	for _, bidderRequest := range bidderRequests {
		liveAdapters[i] = bidderRequest.BidderName
		i++
	}
	// Randomize the list of adapters to make the auction more fair
	randomizeList(liveAdapters)

	return liveAdapters
}

func buildStoredAuctionResponse(storedAuctionResponses map[string]json.RawMessage) (map[openrtb_ext.BidderName]*pbsOrtbSeatBid, []openrtb_ext.BidderName, error) {

	adapterBids := make(map[openrtb_ext.BidderName]*pbsOrtbSeatBid, 0)
	liveAdapters := make([]openrtb_ext.BidderName, 0)
	for impId, storedResp := range storedAuctionResponses {
		var seatBids []openrtb2.SeatBid

		if err := json.Unmarshal(storedResp, &seatBids); err != nil {
			return nil, nil, err
		}
		for _, seat := range seatBids {
			var bidsToAdd []*pbsOrtbBid
			//set imp id from request
			for i := range seat.Bid {
				seat.Bid[i].ImpID = impId
				bidsToAdd = append(bidsToAdd, &pbsOrtbBid{bid: &seat.Bid[i]})
			}

			bidderName := openrtb_ext.BidderName(seat.Seat)

			if _, ok := adapterBids[bidderName]; ok {
				adapterBids[bidderName].bids = append(adapterBids[bidderName].bids, bidsToAdd...)

			} else {
				//create new seat bid and add it to live adapters
				liveAdapters = append(liveAdapters, bidderName)
				newSeatBid := pbsOrtbSeatBid{bidsToAdd, "", nil, ""}
				adapterBids[bidderName] = &newSeatBid

			}
		}
	}

	return adapterBids, liveAdapters, nil
}

func isAdsCertEnabled(experiment *openrtb_ext.Experiment, info config.BidderInfo) bool {
	requestAdsCertEnabled := experiment != nil && experiment.AdsCert != nil && experiment.AdsCert.Enabled
	bidderAdsCertEnabled := info.Experiment.AdsCert.Enabled
	return requestAdsCertEnabled && bidderAdsCertEnabled
}
