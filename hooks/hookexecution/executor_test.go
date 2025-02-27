package hookexecution

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/prebid/openrtb/v17/openrtb2"
	"github.com/prebid/prebid-server/config"
	"github.com/prebid/prebid-server/hooks"
	"github.com/prebid/prebid-server/hooks/hookanalytics"
	"github.com/prebid/prebid-server/hooks/hookstage"
	"github.com/prebid/prebid-server/metrics"
	metricsConfig "github.com/prebid/prebid-server/metrics/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestEmptyHookExecutor(t *testing.T) {
	executor := EmptyHookExecutor{}
	executor.SetAccount(&config.Account{})

	body := []byte(`{"foo": "bar"}`)
	reader := bytes.NewReader(body)
	req, err := http.NewRequest(http.MethodPost, "https://prebid.com/openrtb2/auction", reader)
	assert.NoError(t, err, "Failed to create http request.")

	entrypointBody, entrypointRejectErr := executor.ExecuteEntrypointStage(req, body)
	rawAuctionBody, rawAuctionRejectErr := executor.ExecuteRawAuctionStage(body)
	processedAuctionRejectErr := executor.ExecuteProcessedAuctionStage(&openrtb2.BidRequest{})

	outcomes := executor.GetOutcomes()
	assert.Equal(t, EmptyHookExecutor{}, executor, "EmptyHookExecutor shouldn't be changed.")
	assert.Empty(t, outcomes, "EmptyHookExecutor shouldn't return stage outcomes.")

	assert.Nil(t, entrypointRejectErr, "EmptyHookExecutor shouldn't return reject error at entrypoint stage.")
	assert.Equal(t, body, entrypointBody, "EmptyHookExecutor shouldn't change body at entrypoint stage.")

	assert.Nil(t, rawAuctionRejectErr, "EmptyHookExecutor shouldn't return reject error at raw-auction stage.")
	assert.Equal(t, body, rawAuctionBody, "EmptyHookExecutor shouldn't change body at raw-auction stage.")

	assert.Nil(t, processedAuctionRejectErr, "EmptyHookExecutor shouldn't return reject error at processed-auction stage.")
}

func TestExecuteEntrypointStage(t *testing.T) {
	const body string = `{"name": "John", "last_name": "Doe"}`
	const urlString string = "https://prebid.com/openrtb2/auction"

	foobarModuleCtx := &moduleContexts{ctxs: map[string]hookstage.ModuleContext{"foobar": nil}}

	testCases := []struct {
		description            string
		givenBody              string
		givenUrl               string
		givenPlanBuilder       hooks.ExecutionPlanBuilder
		expectedBody           string
		expectedHeader         http.Header
		expectedQuery          url.Values
		expectedReject         *RejectError
		expectedModuleContexts *moduleContexts
		expectedStageOutcomes  []StageOutcome
	}{
		{
			description:            "Payload not changed if hook execution plan empty",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       hooks.EmptyPlanBuilder{},
			expectedBody:           body,
			expectedHeader:         http.Header{},
			expectedQuery:          url.Values{},
			expectedReject:         nil,
			expectedModuleContexts: &moduleContexts{ctxs: map[string]hookstage.ModuleContext{}},
			expectedStageOutcomes:  []StageOutcome{},
		},
		{
			description:            "Payload changed if hooks return mutations",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       TestApplyHookMutationsBuilder{},
			expectedBody:           `{"last_name": "Doe", "foo": "bar"}`,
			expectedHeader:         http.Header{"Foo": []string{"bar"}},
			expectedQuery:          url.Values{"foo": []string{"baz"}},
			expectedReject:         nil,
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityHttpRequest,
					Stage:  hooks.StageEntrypoint.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{fmt.Sprintf("Hook mutation successfully applied, affected key: header.foo, mutation type: %s", hookstage.MutationUpdate)},
									Errors:        nil,
									Warnings:      nil,
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foobaz"},
									Status:        StatusExecutionFailure,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      []string{"failed to apply hook mutation: key not found"},
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "bar"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{fmt.Sprintf("Hook mutation successfully applied, affected key: param.foo, mutation type: %s", hookstage.MutationUpdate)},
									Errors:        nil,
									Warnings:      nil,
								},
							},
						},
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "baz"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.foo, mutation type: %s", hookstage.MutationUpdate),
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.name, mutation type: %s", hookstage.MutationDelete),
									},
									Errors:   nil,
									Warnings: nil,
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusFailure,
									Action:        "",
									Message:       "",
									DebugMessages: nil,
									Errors:        []string{"hook execution failed: attribute not found"},
									Warnings:      nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:            "Stage execution can be rejected - and later hooks rejected",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       TestRejectPlanBuilder{},
			expectedBody:           body,
			expectedHeader:         http.Header{"Foo": []string{"bar"}},
			expectedQuery:          url.Values{},
			expectedReject:         &RejectError{0, HookID{ModuleCode: "foobar", HookImplCode: "bar"}, hooks.StageEntrypoint.String()},
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityHttpRequest,
					Stage:  hooks.StageEntrypoint.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: header.foo, mutation type: %s", hookstage.MutationUpdate),
									},
									Errors:   nil,
									Warnings: nil,
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "baz"},
									Status:        StatusExecutionFailure,
									Action:        "",
									Message:       "",
									DebugMessages: nil,
									Errors:        []string{"unexpected error"},
									Warnings:      nil,
								},
							},
						},
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "bar"},
									Status:        StatusSuccess,
									Action:        ActionReject,
									Message:       "",
									DebugMessages: nil,
									Errors: []string{
										`Module foobar (hook: bar) rejected request with code 0 at entrypoint stage`,
									},
									Warnings: nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:            "Stage execution can be timed out",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       TestWithTimeoutPlanBuilder{},
			expectedBody:           `{"foo":"bar", "last_name":"Doe"}`,
			expectedHeader:         http.Header{"Foo": []string{"bar"}},
			expectedQuery:          url.Values{},
			expectedReject:         nil,
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityHttpRequest,
					Stage:  hooks.StageEntrypoint.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: header.foo, mutation type: %s", hookstage.MutationUpdate),
									},
									Errors:   nil,
									Warnings: nil,
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "bar"},
									Status:        StatusTimeout,
									Action:        "",
									Message:       "",
									DebugMessages: nil,
									Errors:        []string{"Hook execution timeout"},
									Warnings:      nil,
								},
							},
						},
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "baz"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.foo, mutation type: %s", hookstage.MutationUpdate),
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.name, mutation type: %s", hookstage.MutationDelete),
									},
									Errors:   nil,
									Warnings: nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:      "Modules contexts are preserved and correct",
			givenBody:        body,
			givenUrl:         urlString,
			givenPlanBuilder: TestWithModuleContextsPlanBuilder{},
			expectedBody:     body,
			expectedHeader:   http.Header{},
			expectedQuery:    url.Values{},
			expectedReject:   nil,
			expectedModuleContexts: &moduleContexts{ctxs: map[string]hookstage.ModuleContext{
				"module-1": {"entrypoint-ctx-1": "some-ctx-1", "entrypoint-ctx-3": "some-ctx-3"},
				"module-2": {"entrypoint-ctx-2": "some-ctx-2"},
			}},
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityHttpRequest,
					Stage:  hooks.StageEntrypoint.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-1", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
							},
						},
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-2", HookImplCode: "bar"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-1", HookImplCode: "baz"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
							},
						},
					},
				},
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.description, func(t *testing.T) {
			body := []byte(test.givenBody)
			reader := bytes.NewReader(body)
			req, err := http.NewRequest(http.MethodPost, test.givenUrl, reader)
			assert.NoError(t, err)

			exec := NewHookExecutor(test.givenPlanBuilder, EndpointAuction, &metricsConfig.NilMetricsEngine{})
			newBody, reject := exec.ExecuteEntrypointStage(req, body)

			assert.Equal(t, test.expectedReject, reject, "Unexpected stage reject.")
			assert.JSONEq(t, test.expectedBody, string(newBody), "Incorrect request body.")
			assert.Equal(t, test.expectedHeader, req.Header, "Incorrect request header.")
			assert.Equal(t, test.expectedQuery, req.URL.Query(), "Incorrect request query.")
			assert.Equal(t, test.expectedModuleContexts, exec.moduleContexts, "Incorrect module contexts")

			stageOutcomes := exec.GetOutcomes()
			if len(test.expectedStageOutcomes) == 0 {
				assert.Empty(t, stageOutcomes, "Incorrect stage outcomes.")
			} else {
				assertEqualStageOutcomes(t, test.expectedStageOutcomes[0], stageOutcomes[0])
			}
		})
	}
}

func TestMetricsAreGatheredDuringHookExecution(t *testing.T) {
	reader := bytes.NewReader(nil)
	req, err := http.NewRequest(http.MethodPost, "https://prebid.com/openrtb2/auction", reader)
	assert.NoError(t, err)

	metricEngine := &metrics.MetricsEngineMock{}
	builder := TestAllHookResultsBuilder{}
	exec := NewHookExecutor(TestAllHookResultsBuilder{}, "/openrtb2/auction", metricEngine)
	moduleLabels := metrics.ModuleLabels{
		Module: "module-1",
		Stage:  "entrypoint",
	}
	rTime := func(dur time.Duration) bool { return dur.Nanoseconds() > 0 }
	plan := builder.PlanForEntrypointStage("")
	hooksCalledDuringStage := 0
	for _, group := range plan {
		for range group.Hooks {
			hooksCalledDuringStage++
		}
	}
	metricEngine.On("RecordModuleCalled", moduleLabels, mock.MatchedBy(rTime)).Times(hooksCalledDuringStage)
	metricEngine.On("RecordModuleSuccessUpdated", moduleLabels).Once()
	metricEngine.On("RecordModuleSuccessRejected", moduleLabels).Once()
	metricEngine.On("RecordModuleTimeout", moduleLabels).Once()
	metricEngine.On("RecordModuleExecutionError", moduleLabels).Twice()
	metricEngine.On("RecordModuleFailed", moduleLabels).Once()
	metricEngine.On("RecordModuleSuccessNooped", moduleLabels).Once()

	_, _ = exec.ExecuteEntrypointStage(req, nil)

	// Assert that all module metrics funcs were called with the parameters we expected
	metricEngine.AssertExpectations(t)
}

func TestExecuteRawAuctionStage(t *testing.T) {
	const body string = `{"name": "John", "last_name": "Doe"}`
	const bodyUpdated string = `{"last_name": "Doe", "foo": "bar"}`
	const urlString string = "https://prebid.com/openrtb2/auction"

	foobarModuleCtx := &moduleContexts{ctxs: map[string]hookstage.ModuleContext{"foobar": nil}}
	account := &config.Account{}

	testCases := []struct {
		description            string
		givenBody              string
		givenUrl               string
		givenPlanBuilder       hooks.ExecutionPlanBuilder
		givenAccount           *config.Account
		expectedBody           string
		expectedReject         *RejectError
		expectedModuleContexts *moduleContexts
		expectedStageOutcomes  []StageOutcome
	}{
		{
			description:            "Payload not changed if hook execution plan empty",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       hooks.EmptyPlanBuilder{},
			givenAccount:           account,
			expectedBody:           body,
			expectedReject:         nil,
			expectedModuleContexts: &moduleContexts{ctxs: map[string]hookstage.ModuleContext{}},
			expectedStageOutcomes:  []StageOutcome{},
		},
		{
			description:            "Payload changed if hooks return mutations",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       TestApplyHookMutationsBuilder{},
			givenAccount:           account,
			expectedBody:           bodyUpdated,
			expectedReject:         nil,
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityAuctionRequest,
					Stage:  hooks.StageRawAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.foo, mutation type: %s", hookstage.MutationUpdate),
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.name, mutation type: %s", hookstage.MutationDelete),
									},
									Errors:   nil,
									Warnings: nil,
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "bar"},
									Status:        StatusExecutionFailure,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      []string{"failed to apply hook mutation: key not found"},
								},
							},
						},
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "baz"},
									Status:        StatusFailure,
									Action:        "",
									Message:       "",
									DebugMessages: nil,
									Errors:        []string{"hook execution failed: attribute not found"},
									Warnings:      nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:            "Stage execution can be rejected - and later hooks rejected",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       TestRejectPlanBuilder{},
			givenAccount:           nil,
			expectedBody:           bodyUpdated,
			expectedReject:         &RejectError{0, HookID{ModuleCode: "foobar", HookImplCode: "bar"}, hooks.StageRawAuctionRequest.String()},
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					ExecutionTime: ExecutionTime{},
					Entity:        entityAuctionRequest,
					Stage:         hooks.StageRawAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							ExecutionTime: ExecutionTime{},
							InvocationResults: []HookOutcome{
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.foo, mutation type: %s", hookstage.MutationUpdate),
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.name, mutation type: %s", hookstage.MutationDelete),
									},
									Errors:   nil,
									Warnings: nil,
								},
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "baz"},
									Status:        StatusExecutionFailure,
									Action:        "",
									Message:       "",
									DebugMessages: nil,
									Errors:        []string{"unexpected error"},
									Warnings:      nil,
								},
							},
						},
						{
							ExecutionTime: ExecutionTime{},
							InvocationResults: []HookOutcome{
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "bar"},
									Status:        StatusSuccess,
									Action:        ActionReject,
									Message:       "",
									DebugMessages: nil,
									Errors: []string{
										`Module foobar (hook: bar) rejected request with code 0 at raw_auction_request stage`,
									},
									Warnings: nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:            "Stage execution can be timed out",
			givenBody:              body,
			givenUrl:               urlString,
			givenPlanBuilder:       TestWithTimeoutPlanBuilder{},
			givenAccount:           account,
			expectedBody:           bodyUpdated,
			expectedReject:         nil,
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					ExecutionTime: ExecutionTime{},
					Entity:        entityAuctionRequest,
					Stage:         hooks.StageRawAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							ExecutionTime: ExecutionTime{},
							InvocationResults: []HookOutcome{
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.foo, mutation type: %s", hookstage.MutationUpdate),
										fmt.Sprintf("Hook mutation successfully applied, affected key: body.name, mutation type: %s", hookstage.MutationDelete),
									},
									Errors:   nil,
									Warnings: nil,
								},
							},
						},
						{
							ExecutionTime: ExecutionTime{},
							InvocationResults: []HookOutcome{
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "bar"},
									Status:        StatusTimeout,
									Action:        "",
									Message:       "",
									DebugMessages: nil,
									Errors:        []string{"Hook execution timeout"},
									Warnings:      nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:      "Modules contexts are preserved and correct",
			givenBody:        body,
			givenUrl:         urlString,
			givenPlanBuilder: TestWithModuleContextsPlanBuilder{},
			givenAccount:     account,
			expectedBody:     body,
			expectedReject:   nil,
			expectedModuleContexts: &moduleContexts{ctxs: map[string]hookstage.ModuleContext{
				"module-1": {"raw-auction-ctx-1": "some-ctx-1", "raw-auction-ctx-3": "some-ctx-3"},
				"module-2": {"raw-auction-ctx-2": "some-ctx-2"},
			}},
			expectedStageOutcomes: []StageOutcome{
				{
					ExecutionTime: ExecutionTime{},
					Entity:        entityAuctionRequest,
					Stage:         hooks.StageRawAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							ExecutionTime: ExecutionTime{},
							InvocationResults: []HookOutcome{
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-1", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-2", HookImplCode: "baz"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
							},
						},
						{
							ExecutionTime: ExecutionTime{},
							InvocationResults: []HookOutcome{
								{
									ExecutionTime: ExecutionTime{},
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-1", HookImplCode: "bar"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
							},
						},
					},
				},
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.description, func(t *testing.T) {
			exec := NewHookExecutor(test.givenPlanBuilder, EndpointAuction, &metricsConfig.NilMetricsEngine{})
			exec.SetAccount(test.givenAccount)

			newBody, reject := exec.ExecuteRawAuctionStage([]byte(test.givenBody))

			assert.Equal(t, test.expectedReject, reject, "Unexpected stage reject.")
			assert.JSONEq(t, test.expectedBody, string(newBody), "Incorrect request body.")
			assert.Equal(t, test.expectedModuleContexts, exec.moduleContexts, "Incorrect module contexts")

			stageOutcomes := exec.GetOutcomes()
			if len(test.expectedStageOutcomes) == 0 {
				assert.Empty(t, stageOutcomes, "Incorrect stage outcomes.")
			} else {
				assertEqualStageOutcomes(t, test.expectedStageOutcomes[0], stageOutcomes[0])
			}
		})
	}
}

func TestExecuteProcessedAuctionStage(t *testing.T) {
	foobarModuleCtx := &moduleContexts{ctxs: map[string]hookstage.ModuleContext{"foobar": nil}}
	account := &config.Account{}
	req := openrtb2.BidRequest{ID: "some-id", User: &openrtb2.User{ID: "user-id"}}
	reqUpdated := openrtb2.BidRequest{ID: "some-id", User: &openrtb2.User{ID: "user-id", Yob: 2000, Consent: "true"}}

	testCases := []struct {
		description            string
		givenPlanBuilder       hooks.ExecutionPlanBuilder
		givenAccount           *config.Account
		givenRequest           openrtb2.BidRequest
		expectedRequest        openrtb2.BidRequest
		expectedReject         *RejectError
		expectedModuleContexts *moduleContexts
		expectedStageOutcomes  []StageOutcome
	}{
		{
			description:            "Request not changed if hook execution plan empty",
			givenPlanBuilder:       hooks.EmptyPlanBuilder{},
			givenAccount:           account,
			givenRequest:           req,
			expectedRequest:        req,
			expectedReject:         nil,
			expectedModuleContexts: &moduleContexts{ctxs: map[string]hookstage.ModuleContext{}},
			expectedStageOutcomes:  []StageOutcome{},
		},
		{
			description:            "Request changed if hooks return mutations",
			givenPlanBuilder:       TestApplyHookMutationsBuilder{},
			givenAccount:           account,
			givenRequest:           req,
			expectedRequest:        reqUpdated,
			expectedReject:         nil,
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityAuctionRequest,
					Stage:  hooks.StageProcessedAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: bidRequest.user.yob, mutation type: %s", hookstage.MutationUpdate),
										fmt.Sprintf("Hook mutation successfully applied, affected key: bidRequest.user.consent, mutation type: %s", hookstage.MutationUpdate),
									},
									Errors:   nil,
									Warnings: nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:            "Stage execution can be rejected - and later hooks rejected",
			givenPlanBuilder:       TestRejectPlanBuilder{},
			givenAccount:           nil,
			givenRequest:           req,
			expectedRequest:        req,
			expectedReject:         &RejectError{0, HookID{ModuleCode: "foobar", HookImplCode: "foo"}, hooks.StageProcessedAuctionRequest.String()},
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityAuctionRequest,
					Stage:  hooks.StageProcessedAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionReject,
									Message:       "",
									DebugMessages: nil,
									Errors: []string{
										`Module foobar (hook: foo) rejected request with code 0 at processed_auction_request stage`,
									},
									Warnings: nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:            "Request can be changed when a hook times out",
			givenPlanBuilder:       TestWithTimeoutPlanBuilder{},
			givenAccount:           account,
			givenRequest:           req,
			expectedRequest:        reqUpdated,
			expectedReject:         nil,
			expectedModuleContexts: foobarModuleCtx,
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityAuctionRequest,
					Stage:  hooks.StageProcessedAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "foo"},
									Status:        StatusTimeout,
									Action:        "",
									Message:       "",
									DebugMessages: nil,
									Errors:        []string{"Hook execution timeout"},
									Warnings:      nil,
								},
							},
						},
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "foobar", HookImplCode: "bar"},
									Status:        StatusSuccess,
									Action:        ActionUpdate,
									Message:       "",
									DebugMessages: []string{
										fmt.Sprintf("Hook mutation successfully applied, affected key: bidRequest.user.yob, mutation type: %s", hookstage.MutationUpdate),
										fmt.Sprintf("Hook mutation successfully applied, affected key: bidRequest.user.consent, mutation type: %s", hookstage.MutationUpdate),
									},
									Errors:   nil,
									Warnings: nil,
								},
							},
						},
					},
				},
			},
		},
		{
			description:      "Modules contexts are preserved and correct",
			givenPlanBuilder: TestWithModuleContextsPlanBuilder{},
			givenAccount:     account,
			givenRequest:     req,
			expectedRequest:  req,
			expectedReject:   nil,
			expectedModuleContexts: &moduleContexts{ctxs: map[string]hookstage.ModuleContext{
				"module-1": {"processed-auction-ctx-1": "some-ctx-1", "processed-auction-ctx-3": "some-ctx-3"},
				"module-2": {"processed-auction-ctx-2": "some-ctx-2"},
			}},
			expectedStageOutcomes: []StageOutcome{
				{
					Entity: entityAuctionRequest,
					Stage:  hooks.StageProcessedAuctionRequest.String(),
					Groups: []GroupOutcome{
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-1", HookImplCode: "foo"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
							},
						},
						{
							InvocationResults: []HookOutcome{
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-2", HookImplCode: "bar"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
								{
									AnalyticsTags: hookanalytics.Analytics{},
									HookID:        HookID{ModuleCode: "module-1", HookImplCode: "baz"},
									Status:        StatusSuccess,
									Action:        ActionNone,
									Message:       "",
									DebugMessages: nil,
									Errors:        nil,
									Warnings:      nil,
								},
							},
						},
					},
				},
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.description, func(ti *testing.T) {
			exec := NewHookExecutor(test.givenPlanBuilder, EndpointAuction, &metricsConfig.NilMetricsEngine{})
			exec.SetAccount(test.givenAccount)

			reject := exec.ExecuteProcessedAuctionStage(&test.givenRequest)

			assert.Equal(ti, test.expectedReject, reject, "Unexpected stage reject.")
			assert.Equal(ti, test.expectedRequest, test.givenRequest, "Incorrect request update.")
			assert.Equal(ti, test.expectedModuleContexts, exec.moduleContexts, "Incorrect module contexts")

			stageOutcomes := exec.GetOutcomes()
			if len(test.expectedStageOutcomes) == 0 {
				assert.Empty(ti, stageOutcomes, "Incorrect stage outcomes.")
			} else {
				assertEqualStageOutcomes(ti, test.expectedStageOutcomes[0], stageOutcomes[0])
			}
		})
	}
}

func TestInterStageContextCommunication(t *testing.T) {
	body := []byte(`{"foo": "bar"}`)
	reader := bytes.NewReader(body)
	exec := NewHookExecutor(TestWithModuleContextsPlanBuilder{}, EndpointAuction, &metricsConfig.NilMetricsEngine{})
	req, err := http.NewRequest(http.MethodPost, "https://prebid.com/openrtb2/auction", reader)
	assert.NoError(t, err)

	// test that context added at the entrypoint stage
	_, reject := exec.ExecuteEntrypointStage(req, body)
	assert.Nil(t, reject, "Unexpected reject from entrypoint stage.")
	assert.Equal(
		t,
		&moduleContexts{ctxs: map[string]hookstage.ModuleContext{
			"module-1": {
				"entrypoint-ctx-1": "some-ctx-1",
				"entrypoint-ctx-3": "some-ctx-3",
			},
			"module-2": {"entrypoint-ctx-2": "some-ctx-2"},
		}},
		exec.moduleContexts,
		"Wrong module contexts after executing entrypoint hook.",
	)

	// test that context added at the raw-auction stage merged with existing module contexts
	_, reject = exec.ExecuteRawAuctionStage(body)
	assert.Nil(t, reject, "Unexpected reject from raw-auction stage.")
	assert.Equal(t, &moduleContexts{ctxs: map[string]hookstage.ModuleContext{
		"module-1": {
			"entrypoint-ctx-1":  "some-ctx-1",
			"entrypoint-ctx-3":  "some-ctx-3",
			"raw-auction-ctx-1": "some-ctx-1",
			"raw-auction-ctx-3": "some-ctx-3",
		},
		"module-2": {
			"entrypoint-ctx-2":  "some-ctx-2",
			"raw-auction-ctx-2": "some-ctx-2",
		},
	}}, exec.moduleContexts, "Wrong module contexts after executing raw-auction hook.")

	// test that context added at the processed-auction stage merged with existing module contexts
	reject = exec.ExecuteProcessedAuctionStage(&openrtb2.BidRequest{})
	assert.Nil(t, reject, "Unexpected reject from processed-auction stage.")
	assert.Equal(t, &moduleContexts{ctxs: map[string]hookstage.ModuleContext{
		"module-1": {
			"entrypoint-ctx-1":        "some-ctx-1",
			"entrypoint-ctx-3":        "some-ctx-3",
			"raw-auction-ctx-1":       "some-ctx-1",
			"raw-auction-ctx-3":       "some-ctx-3",
			"processed-auction-ctx-1": "some-ctx-1",
			"processed-auction-ctx-3": "some-ctx-3",
		},
		"module-2": {
			"entrypoint-ctx-2":        "some-ctx-2",
			"raw-auction-ctx-2":       "some-ctx-2",
			"processed-auction-ctx-2": "some-ctx-2",
		},
	}}, exec.moduleContexts, "Wrong module contexts after executing processed-auction hook.")
}

type TestApplyHookMutationsBuilder struct {
	hooks.EmptyPlanBuilder
}

func (e TestApplyHookMutationsBuilder) PlanForEntrypointStage(_ string) hooks.Plan[hookstage.Entrypoint] {
	return hooks.Plan[hookstage.Entrypoint]{
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateHeaderEntrypointHook{}},
				{Module: "foobar", Code: "foobaz", Hook: mockFailedMutationHook{}},
				{Module: "foobar", Code: "bar", Hook: mockUpdateQueryEntrypointHook{}},
			},
		},
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "foobar", Code: "baz", Hook: mockUpdateBodyHook{}},
				{Module: "foobar", Code: "foo", Hook: mockFailureHook{}},
			},
		},
	}
}

func (e TestApplyHookMutationsBuilder) PlanForRawAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.RawAuctionRequest] {
	return hooks.Plan[hookstage.RawAuctionRequest]{
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateBodyHook{}},
				{Module: "foobar", Code: "bar", Hook: mockFailedMutationHook{}},
			},
		},
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "foobar", Code: "baz", Hook: mockFailureHook{}},
			},
		},
	}
}

func (e TestApplyHookMutationsBuilder) PlanForProcessedAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.ProcessedAuctionRequest] {
	return hooks.Plan[hookstage.ProcessedAuctionRequest]{
		hooks.Group[hookstage.ProcessedAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateBidRequestHook{}},
			},
		},
	}
}

type TestRejectPlanBuilder struct {
	hooks.EmptyPlanBuilder
}

func (e TestRejectPlanBuilder) PlanForEntrypointStage(_ string) hooks.Plan[hookstage.Entrypoint] {
	return hooks.Plan[hookstage.Entrypoint]{
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateHeaderEntrypointHook{}},
				{Module: "foobar", Code: "baz", Hook: mockErrorHook{}},
			},
		},
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 5 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				// reject stage
				{Module: "foobar", Code: "bar", Hook: mockRejectHook{}},
				// next hook rejected: we use timeout hook to make sure
				// that it runs longer than previous one, so it won't be executed earlier
				{Module: "foobar", Code: "baz", Hook: mockTimeoutHook{}},
			},
		},
		// group of hooks rejected
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateHeaderEntrypointHook{}},
				{Module: "foobar", Code: "baz", Hook: mockErrorHook{}},
			},
		},
	}
}

func (e TestRejectPlanBuilder) PlanForRawAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.RawAuctionRequest] {
	return hooks.Plan[hookstage.RawAuctionRequest]{
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateBodyHook{}},
				{Module: "foobar", Code: "baz", Hook: mockErrorHook{}},
			},
		},
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "foobar", Code: "bar", Hook: mockRejectHook{}},
				// next hook rejected: we use timeout hook to make sure
				// that it runs longer than previous one, so it won't be executed earlier
				{Module: "foobar", Code: "baz", Hook: mockTimeoutHook{}},
			},
		},
		// group of hooks rejected
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateBodyHook{}},
				{Module: "foobar", Code: "baz", Hook: mockErrorHook{}},
			},
		},
	}
}

func (e TestRejectPlanBuilder) PlanForProcessedAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.ProcessedAuctionRequest] {
	return hooks.Plan[hookstage.ProcessedAuctionRequest]{
		hooks.Group[hookstage.ProcessedAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{
				{Module: "foobar", Code: "foo", Hook: mockRejectHook{}},
			},
		},
		hooks.Group[hookstage.ProcessedAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{
				{Module: "foobar", Code: "bar", Hook: mockUpdateBidRequestHook{}},
			},
		},
	}
}

type TestWithTimeoutPlanBuilder struct {
	hooks.EmptyPlanBuilder
}

func (e TestWithTimeoutPlanBuilder) PlanForEntrypointStage(_ string) hooks.Plan[hookstage.Entrypoint] {
	return hooks.Plan[hookstage.Entrypoint]{
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateHeaderEntrypointHook{}},
				{Module: "foobar", Code: "bar", Hook: mockTimeoutHook{}},
			},
		},
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "foobar", Code: "baz", Hook: mockUpdateBodyHook{}},
			},
		},
	}
}

func (e TestWithTimeoutPlanBuilder) PlanForRawAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.RawAuctionRequest] {
	return hooks.Plan[hookstage.RawAuctionRequest]{
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "foobar", Code: "foo", Hook: mockUpdateBodyHook{}},
			},
		},
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "foobar", Code: "bar", Hook: mockTimeoutHook{}},
			},
		},
	}
}

func (e TestWithTimeoutPlanBuilder) PlanForProcessedAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.ProcessedAuctionRequest] {
	return hooks.Plan[hookstage.ProcessedAuctionRequest]{
		hooks.Group[hookstage.ProcessedAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{
				{Module: "foobar", Code: "foo", Hook: mockTimeoutHook{}},
			},
		},
		hooks.Group[hookstage.ProcessedAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{
				{Module: "foobar", Code: "bar", Hook: mockUpdateBidRequestHook{}},
			},
		},
	}
}

type TestWithModuleContextsPlanBuilder struct {
	hooks.EmptyPlanBuilder
}

func (e TestWithModuleContextsPlanBuilder) PlanForEntrypointStage(_ string) hooks.Plan[hookstage.Entrypoint] {
	return hooks.Plan[hookstage.Entrypoint]{
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "module-1", Code: "foo", Hook: mockModuleContextHook{key: "entrypoint-ctx-1", val: "some-ctx-1"}},
			},
		},
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "module-2", Code: "bar", Hook: mockModuleContextHook{key: "entrypoint-ctx-2", val: "some-ctx-2"}},
				{Module: "module-1", Code: "baz", Hook: mockModuleContextHook{key: "entrypoint-ctx-3", val: "some-ctx-3"}},
			},
		},
	}
}

func (e TestWithModuleContextsPlanBuilder) PlanForRawAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.RawAuctionRequest] {
	return hooks.Plan[hookstage.RawAuctionRequest]{
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "module-1", Code: "foo", Hook: mockModuleContextHook{key: "raw-auction-ctx-1", val: "some-ctx-1"}},
				{Module: "module-2", Code: "baz", Hook: mockModuleContextHook{key: "raw-auction-ctx-2", val: "some-ctx-2"}},
			},
		},
		hooks.Group[hookstage.RawAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.RawAuctionRequest]{
				{Module: "module-1", Code: "bar", Hook: mockModuleContextHook{key: "raw-auction-ctx-3", val: "some-ctx-3"}},
			},
		},
	}
}

func (e TestWithModuleContextsPlanBuilder) PlanForProcessedAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.ProcessedAuctionRequest] {
	return hooks.Plan[hookstage.ProcessedAuctionRequest]{
		hooks.Group[hookstage.ProcessedAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{
				{Module: "module-1", Code: "foo", Hook: mockModuleContextHook{key: "processed-auction-ctx-1", val: "some-ctx-1"}},
			},
		},
		hooks.Group[hookstage.ProcessedAuctionRequest]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{
				{Module: "module-2", Code: "bar", Hook: mockModuleContextHook{key: "processed-auction-ctx-2", val: "some-ctx-2"}},
				{Module: "module-1", Code: "baz", Hook: mockModuleContextHook{key: "processed-auction-ctx-3", val: "some-ctx-3"}},
			},
		},
	}
}

type TestAllHookResultsBuilder struct {
	hooks.EmptyPlanBuilder
}

func (e TestAllHookResultsBuilder) PlanForEntrypointStage(_ string) hooks.Plan[hookstage.Entrypoint] {
	return hooks.Plan[hookstage.Entrypoint]{
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 1 * time.Millisecond,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "module-1", Code: "code-1", Hook: mockUpdateHeaderEntrypointHook{}},
				{Module: "module-1", Code: "code-3", Hook: mockTimeoutHook{}},
				{Module: "module-1", Code: "code-4", Hook: mockFailureHook{}},
				{Module: "module-1", Code: "code-5", Hook: mockErrorHook{}},
				{Module: "module-1", Code: "code-6", Hook: mockFailedMutationHook{}},
				{Module: "module-1", Code: "code-7", Hook: mockModuleContextHook{key: "key", val: "val"}},
			},
		},
		// place the reject hook in a separate group because it rejects the stage completely
		// thus we can not make accurate mock calls if it is processed in parallel with others
		hooks.Group[hookstage.Entrypoint]{
			Timeout: 10 * time.Second,
			Hooks: []hooks.HookWrapper[hookstage.Entrypoint]{
				{Module: "module-1", Code: "code-2", Hook: mockRejectHook{}},
			},
		},
	}
}
