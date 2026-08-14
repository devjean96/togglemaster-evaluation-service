package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func newEvaluationApp(t *testing.T, transport http.RoundTripper) (*App, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	if transport == nil {
		transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusNotFound, `{}`), nil
		})
	}
	return &App{
		RedisClient:         client,
		HttpClient:          &http.Client{Transport: transport},
		FlagServiceURL:      "http://flag-service",
		TargetingServiceURL: "http://targeting-service",
	}, server
}

func TestGetDeterministicBucket(t *testing.T) {
	first := getDeterministicBucket("user-1checkout")
	second := getDeterministicBucket("user-1checkout")

	if first != second || first < 0 || first > 99 {
		t.Fatalf("expected stable bucket from 0 to 99, got %d and %d", first, second)
	}
}

func TestRunEvaluationLogic(t *testing.T) {
	app := &App{}
	enabledFlag := &Flag{Name: "checkout", IsEnabled: true}
	userID := "user-1"
	bucket := getDeterministicBucket(userID + enabledFlag.Name)

	tests := []struct {
		name     string
		info     *CombinedFlagInfo
		expected bool
	}{
		{name: "missing flag", info: &CombinedFlagInfo{}, expected: false},
		{name: "disabled flag", info: &CombinedFlagInfo{Flag: &Flag{}}, expected: false},
		{name: "no rule", info: &CombinedFlagInfo{Flag: enabledFlag}, expected: true},
		{
			name:     "disabled rule",
			info:     &CombinedFlagInfo{Flag: enabledFlag, Rule: &TargetingRule{}},
			expected: true,
		},
		{
			name: "included in percentage",
			info: &CombinedFlagInfo{
				Flag: enabledFlag,
				Rule: &TargetingRule{IsEnabled: true, Rules: Rule{Type: "PERCENTAGE", Value: float64(bucket + 1)}},
			},
			expected: true,
		},
		{
			name: "excluded from percentage",
			info: &CombinedFlagInfo{
				Flag: enabledFlag,
				Rule: &TargetingRule{IsEnabled: true, Rules: Rule{Type: "PERCENTAGE", Value: float64(bucket)}},
			},
			expected: false,
		},
		{
			name: "invalid percentage",
			info: &CombinedFlagInfo{
				Flag: enabledFlag,
				Rule: &TargetingRule{IsEnabled: true, Rules: Rule{Type: "PERCENTAGE", Value: "50"}},
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := app.runEvaluationLogic(test.info, userID); actual != test.expected {
				t.Fatalf("expected %t, got %t", test.expected, actual)
			}
		})
	}
}

func TestFetchFlag(t *testing.T) {
	t.Setenv("SERVICE_API_KEY", "service-key")
	var authorization string
	app, _ := newEvaluationApp(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		return jsonResponse(http.StatusOK, `{"id":1,"name":"checkout","is_enabled":true}`), nil
	}))

	flag, err := app.fetchFlag("checkout")

	if err != nil || flag.Name != "checkout" || !flag.IsEnabled {
		t.Fatalf("unexpected flag result: %+v, %v", flag, err)
	}
	if authorization != "Bearer service-key" {
		t.Fatalf("unexpected authorization header: %s", authorization)
	}
}

func TestFetchFlagErrors(t *testing.T) {
	tests := []struct {
		name      string
		transport http.RoundTripper
		notFound  bool
	}{
		{name: "network", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline")
		})},
		{name: "not found", notFound: true, transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusNotFound, `{}`), nil
		})},
		{name: "bad status", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusInternalServerError, `{}`), nil
		})},
		{name: "invalid JSON", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{`), nil
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, _ := newEvaluationApp(t, test.transport)
			_, err := app.fetchFlag("checkout")
			if err == nil {
				t.Fatal("expected an error")
			}
			_, isNotFound := err.(*NotFoundError)
			if isNotFound != test.notFound {
				t.Fatalf("unexpected error type: %T", err)
			}
		})
	}
}

func TestFetchRule(t *testing.T) {
	app, _ := newEvaluationApp(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(
			http.StatusOK,
			`{"flag_name":"checkout","is_enabled":true,"rules":{"type":"PERCENTAGE","value":50}}`,
		), nil
	}))

	rule, err := app.fetchRule("checkout")

	if err != nil || rule.FlagName != "checkout" || rule.Rules.Value.(float64) != 50 {
		t.Fatalf("unexpected rule result: %+v, %v", rule, err)
	}
}

func TestFetchRuleErrors(t *testing.T) {
	tests := []struct {
		name      string
		transport http.RoundTripper
		notFound  bool
	}{
		{name: "network", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline")
		})},
		{name: "not found", notFound: true, transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusNotFound, `{}`), nil
		})},
		{name: "bad status", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusBadGateway, `{}`), nil
		})},
		{name: "invalid JSON", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{`), nil
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, _ := newEvaluationApp(t, test.transport)
			_, err := app.fetchRule("checkout")
			if err == nil {
				t.Fatal("expected an error")
			}
			_, isNotFound := err.(*NotFoundError)
			if isNotFound != test.notFound {
				t.Fatalf("unexpected error type: %T", err)
			}
		})
	}
}

func TestGetCombinedFlagInfoUsesCache(t *testing.T) {
	app, server := newEvaluationApp(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("HTTP services should not be called on a cache hit")
		return nil, nil
	}))
	cached := &CombinedFlagInfo{Flag: &Flag{Name: "checkout", IsEnabled: true}}
	data, _ := json.Marshal(cached)
	server.Set("flag_info:checkout", string(data))

	info, err := app.getCombinedFlagInfo("checkout")

	if err != nil || info.Flag.Name != "checkout" {
		t.Fatalf("unexpected cached result: %+v, %v", info, err)
	}
}

func TestGetCombinedFlagInfoFetchesAndCaches(t *testing.T) {
	app, server := newEvaluationApp(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasPrefix(request.URL.Path, "/flags/") {
			return jsonResponse(http.StatusOK, `{"name":"checkout","is_enabled":true}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}))
	server.Set("flag_info:checkout", "invalid-json")

	info, err := app.getCombinedFlagInfo("checkout")

	if err != nil || info.Flag.Name != "checkout" || info.Rule != nil {
		t.Fatalf("unexpected service result: %+v, %v", info, err)
	}
	if _, err := server.Get("flag_info:checkout"); err != nil {
		t.Fatalf("expected result to be cached: %v", err)
	}
}

func TestGetDecisionReturnsFlagServiceError(t *testing.T) {
	app, _ := newEvaluationApp(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasPrefix(request.URL.Path, "/flags/") {
			return jsonResponse(http.StatusBadGateway, `{}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}))

	if _, err := app.getDecision("user-1", "checkout"); err == nil {
		t.Fatal("expected flag-service error")
	}
}

func TestNotFoundErrorMessage(t *testing.T) {
	err := (&NotFoundError{FlagName: "checkout"}).Error()
	if err != "flag ou regra 'checkout' não encontrada" {
		t.Fatalf("unexpected error message: %s", err)
	}
}
