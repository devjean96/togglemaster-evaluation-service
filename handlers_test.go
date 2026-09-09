package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthHandler(t *testing.T) {
	app := &App{}
	recorder := httptest.NewRecorder()

	app.healthHandler(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected health response: %d, %s", recorder.Code, recorder.Header())
	}
}

func TestEvaluationHandlerRequiresParameters(t *testing.T) {
	app := &App{}
	recorder := httptest.NewRecorder()

	app.evaluationHandler(recorder, httptest.NewRequest(http.MethodGet, "/evaluate", nil))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}
}

func TestEvaluationHandlerRejectsInvalidFlagName(t *testing.T) {
	app := &App{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/evaluate?user_id=user-1&flag_name=../admin", nil)

	app.evaluationHandler(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestEvaluationHandlerReturnsCachedDecision(t *testing.T) {
	app, server := newEvaluationApp(t, nil)
	info := &CombinedFlagInfo{Flag: &Flag{Name: "checkout", IsEnabled: true}}
	data, _ := json.Marshal(info)
	if err := server.Set("flag_info:checkout", string(data)); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/evaluate?user_id=user-1&flag_name=checkout", nil)

	app.evaluationHandler(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response EvaluationResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if response.UserID != "user-1" || response.FlagName != "checkout" || !response.Result {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestEvaluationHandlerFailsSafelyForUnknownFlag(t *testing.T) {
	app, _ := newEvaluationApp(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/evaluate?user_id=user-1&flag_name=unknown", nil)

	app.evaluationHandler(recorder, request)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"result":false`) {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestEvaluationHandlerReturnsBadGatewayForServiceError(t *testing.T) {
	app, _ := newEvaluationApp(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasPrefix(request.URL.Path, "/flags/") {
			return jsonResponse(http.StatusInternalServerError, `{}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/evaluate?user_id=user-1&flag_name=checkout", nil)

	app.evaluationHandler(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d", recorder.Code)
	}
}
