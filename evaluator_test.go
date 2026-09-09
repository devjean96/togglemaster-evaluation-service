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

func TestNewServiceRequestKeepsConfiguredOrigin(t *testing.T) {
	t.Setenv("SERVICE_API_KEY", "service-key")

	request, err := newServiceRequest("https://flag-service.example/api/", "flags", "checkout?#")
	if err != nil {
		t.Fatalf("creating service request: %v", err)
	}
	if request.URL.Scheme != "https" || request.URL.Host != "flag-service.example" {
		t.Fatalf("unexpected request origin: %s", request.URL)
	}
	if request.URL.Path != "/api/flags/checkout?#" || request.URL.RawQuery != "" || request.URL.Fragment != "" {
		t.Fatalf("flag name escaped the URL path: %s", request.URL)
	}
	if request.Header.Get("Authorization") != "Bearer service-key" {
		t.Fatalf("unexpected authorization header: %s", request.Header.Get("Authorization"))
	}
}

func TestNewServiceRequestRejectsInvalidFlagNames(t *testing.T) {
	invalidNames := []string{
		"",
		".",
		"..",
		"path/segment",
		`path\segment`,
		"line\nbreak",
		strings.Repeat("a", maxFlagNameLength+1),
	}

	for _, flagName := range invalidNames {
		t.Run(flagName, func(t *testing.T) {
			_, err := newServiceRequest("http://flag-service", "flags", flagName)
			if !errors.Is(err, errInvalidFlagName) {
				t.Fatalf("expected invalid flag name error, got %v", err)
			}
		})
	}
}

func TestNormalizeServiceBaseURLRejectsUnsafeURLs(t *testing.T) {
	invalidURLs := []string{
		"flag-service",
		"ftp://flag-service",
		"http://user:password@flag-service",
		"http://flag-service?destination=internal",
		"http://flag-service/#fragment",
	}

	for _, serviceURL := range invalidURLs {
		t.Run(serviceURL, func(t *testing.T) {
			if _, err := normalizeServiceBaseURL(serviceURL); err == nil {
				t.Fatal("expected URL validation error")
			}
		})
	}
}

func TestFetchFlagDoesNotFollowRedirects(t *testing.T) {
	requests := 0
	app, _ := newEvaluationApp(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Host != "flag-service" {
			t.Fatalf("request escaped configured host: %s", request.URL)
		}
		response := jsonResponse(http.StatusFound, `{}`)
		response.Header.Set("Location", "http://169.254.169.254/latest/meta-data/")
		return response, nil
	}))

	_, err := app.fetchFlag("checkout")

	if err == nil || requests != 1 {
		t.Fatalf("expected one rejected redirect response, got %d requests and error %v", requests, err)
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
	if err := server.Set("flag_info:checkout", string(data)); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

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
	if err := server.Set("flag_info:checkout", "invalid-json"); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

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
