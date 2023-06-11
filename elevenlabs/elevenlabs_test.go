package elevenlabs_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/haguro/elevenlabs-go/elevenlabs"
)

const (
	mockAPIKey  = "MockAPIKey"
	mockTimeout = 60 * time.Second
)

func testServer(t *testing.T, method string, expectKey bool, queryStr string, statusCode int, respBody []byte, expError error, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			t.Errorf("Server: expected %q, got %q", method, r.Method)
		}
		if expectKey {
			gotAPIKey := r.Header.Get("xi-api-key")
			if gotAPIKey != mockAPIKey {
				t.Errorf("Server: expected API Key %q, got %q", mockAPIKey, gotAPIKey)
			}
		}
		if queryStr != "" {
			gotQuery := r.URL.RawQuery
			if gotQuery != queryStr {
				t.Errorf("Server: expected query string %q, got %q", queryStr, gotQuery)
			}
		}

		if delay > 0 {
			time.Sleep(delay)
		}

		w.WriteHeader(statusCode)
		if expError != nil {
			j, err := json.Marshal(expError)
			if err != nil {
				t.Fatal("Failed to marshal expError")
			}
			w.Write(j)
			return
		}
		w.Write(respBody)
	}))
}

func TestDefaultClientSetup(t *testing.T) {
	baseURL := "http://localhost:1234/"
	defaultClient := elevenlabs.MockDefaultClient(baseURL)
	elevenlabs.SetAPIKey(mockAPIKey)
	elevenlabs.SetTimeout(mockTimeout)
	expected := elevenlabs.NewMockClient(context.Background(), baseURL, mockAPIKey, mockTimeout)
	if !reflect.DeepEqual(expected, defaultClient) {
		t.Errorf("Default client set up is incorrect %+v", defaultClient)
	}
}

func TestRequestTimeout(t *testing.T) {
	server := testServer(t, http.MethodPost, true, "", http.StatusOK, []byte{}, nil, 500*time.Millisecond)
	defer server.Close()
	client := elevenlabs.NewMockClient(context.Background(), server.URL, mockAPIKey, 100*time.Millisecond)
	_, err := client.TextToSpeech("TestVoiceID", elevenlabs.TextToSpeechRequest{})
	if err == nil {
		t.Fatalf("Expected context deadline exceeded error returned, got nil")
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Expected context deadline exceeded error returned, got err")
	}
}
func TestAPIErrorOnBadRequestAndUnauthorized(t *testing.T) {
	for _, code := range [2]int{http.StatusBadRequest, http.StatusUnauthorized} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := testServer(t, http.MethodGet, true, "", code, []byte{}, &elevenlabs.APIError{}, 0)
			defer server.Close()
			client := elevenlabs.NewMockClient(context.Background(), server.URL, mockAPIKey, mockTimeout)
			_, err := client.GetModels()
			if err == nil {
				t.Errorf("Expected error of type %T with status code %d, got nil", &elevenlabs.APIError{}, code)
				return
			}
			if _, ok := err.(*elevenlabs.APIError); !ok {
				t.Errorf("Expected error of type %T with status code %d, got %T: %q", &elevenlabs.APIError{}, code, err, err)
			}
		})
	}
}

func TestValidationErrorOnUnprocessableEntity(t *testing.T) {
	server := testServer(t, http.MethodPost, true, "", http.StatusUnprocessableEntity, []byte{}, &elevenlabs.ValidationError{}, 0)
	defer server.Close()
	client := elevenlabs.NewMockClient(context.Background(), server.URL, mockAPIKey, mockTimeout)
	_, err := client.TextToSpeech("TestVoiceID", elevenlabs.TextToSpeechRequest{})
	if err == nil {
		t.Errorf("Expected error of type %T, got nil", &elevenlabs.ValidationError{})
		return
	}
	if _, ok := err.(*elevenlabs.ValidationError); !ok {
		t.Errorf("Expected error of type %T, got %T: %q", &elevenlabs.ValidationError{}, err, err)
	}
}

func TestTextToSpeech(t *testing.T) {
	testCases := []struct {
		name               string
		excludeAPIKey      bool
		queries            []elevenlabs.QueryFunc
		expQueryString     string
		testRequestBody    any
		expResponseBody    []byte
		expectedRespStatus int
	}{
		{
			name:          "No API key and no queries",
			excludeAPIKey: true,
			testRequestBody: elevenlabs.TextToSpeechRequest{
				ModelID: "model1",
				Text:    "Test text",
			},
			expResponseBody:    []byte("Test audio response"),
			expectedRespStatus: http.StatusOK,
		},
		{
			name:           "With API key and latency optimizations query",
			excludeAPIKey:  false,
			queries:        []elevenlabs.QueryFunc{elevenlabs.LatencyOptimizations(2)},
			expQueryString: "optimize_streaming_latency=2",
			testRequestBody: elevenlabs.TextToSpeechRequest{
				ModelID: "model1",
				Text:    "Test text",
			},
			expResponseBody:    []byte("Test audio response"),
			expectedRespStatus: http.StatusOK,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			requestAPIKey := mockAPIKey
			if tc.excludeAPIKey {
				requestAPIKey = ""
			}
			server := testServer(t, http.MethodPost, false, tc.expQueryString, tc.expectedRespStatus, tc.expResponseBody, nil, 0)
			defer server.Close()

			client := elevenlabs.NewMockClient(context.Background(), server.URL, requestAPIKey, mockTimeout)
			respBody, err := client.TextToSpeech("voiceID", tc.testRequestBody.(elevenlabs.TextToSpeechRequest), tc.queries...)

			if err != nil {
				t.Errorf("Expected no errors, got error: %q", err)
			}

			if string(respBody) != string(tc.expResponseBody) {
				t.Errorf("Expected response %q, got %q", string(tc.expResponseBody), string(respBody))
			}
		})
	}
}

func TestGetModels(t *testing.T) {
	respBody := `
[
	{
		"model_id": "TestModelID",
		"name": "TestModelName",
		"can_be_finetuned": true,
		"can_do_text_to_speech": true,
		"can_do_voice_conversion": true,
		"token_cost_factor": 0,
		"description": "TestModelDescription",
		"languages": [
			{
				"language_id": "LangIDEnglish",
				"name": "English"
			}
		]
	}
]`

	server := testServer(t, http.MethodGet, true, "", http.StatusOK, []byte(respBody), nil, 0)
	defer server.Close()
	client := elevenlabs.NewMockClient(context.Background(), server.URL, mockAPIKey, mockTimeout)
	models, err := client.GetModels()
	if err != nil {
		t.Errorf("Expected no errors from `GetModels`, got error: %q", err)
	}
	if len(models) != 1 {
		t.Fatalf("Expected unmarshalled response to contain exactly one model, got %d", len(models))
	}
	var expModels []elevenlabs.Model
	if err := json.Unmarshal([]byte(respBody), &expModels); err != nil {
		t.Fatalf("Failed to unmarshal test respBody: %s", err)
	}
	if !reflect.DeepEqual(expModels, models) {
		t.Errorf("Unexpected model in response: %+v", models[0])
	}

}