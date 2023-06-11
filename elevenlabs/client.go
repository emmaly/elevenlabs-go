package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	elevenlabsBaseURL = "https://api.elevenlabs.io/v1"
	defaultTimeout    = 30 * time.Second
)

var (
	once          sync.Once
	defaultClient *Client
)

type QueryFunc func(*url.Values)

type Client struct {
	baseURL string
	apiKey  string
	timeout time.Duration
	ctx     context.Context
}

func getDefaultClient() *Client {
	once.Do(func() {
		defaultClient = NewClient(context.Background(), "", defaultTimeout)
	})
	return defaultClient
}

func SetAPIKey(apiKey string) {
	getDefaultClient().apiKey = apiKey
}

func SetTimeout(timeout time.Duration) {
	getDefaultClient().timeout = timeout
}

func TextToSpeech(voiceID string, ttsReq TextToSpeechRequest, queries ...QueryFunc) ([]byte, error) {
	return getDefaultClient().TextToSpeech(voiceID, ttsReq, queries...)
}

func GetModels() ([]Model, error) {
	return getDefaultClient().GetModels()
}

func NewClient(ctx context.Context, apiKey string, reqTimeout time.Duration) *Client {
	return &Client{baseURL: elevenlabsBaseURL, apiKey: apiKey, timeout: reqTimeout, ctx: ctx}
}

func (c *Client) doRequest(ctx context.Context, method, url string, body []byte, queries ...QueryFunc) ([]byte, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(timeoutCtx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	req.Header.Add("accept", "application/json")
	if c.apiKey != "" {
		req.Header.Add("xi-api-key", c.apiKey)
	}

	q := req.URL.Query()
	for _, qf := range queries {
		qf(&q)
	}
	req.URL.RawQuery = q.Encode()

	client := &http.Client{} //TODO customise HTTP client behaviour if some is needed or allow some configuration by API user.
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized:
			apiErr := &APIError{}
			if err := json.Unmarshal(respBody, apiErr); err != nil {
				return respBody, err
			}
			return respBody, apiErr
		case http.StatusUnprocessableEntity:
			valErr := &ValidationError{}
			if err := json.Unmarshal(respBody, valErr); err != nil {
				return respBody, err
			}
			return respBody, valErr
		default:
			return respBody, fmt.Errorf("unexpected http status  \"%d %s\" returned from server", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
	}

	return respBody, nil
}

func LatencyOptimizations(value int) QueryFunc {
	return func(q *url.Values) {
		q.Add("optimize_streaming_latency", fmt.Sprint(value))
	}
}

func (c *Client) TextToSpeech(voiceID string, ttsReq TextToSpeechRequest, queries ...QueryFunc) ([]byte, error) {
	reqBody, err := json.Marshal(ttsReq)
	if err != nil {
		return nil, err
	}

	return c.doRequest(c.ctx, http.MethodPost, fmt.Sprintf("%s/text-to-speech/%s", c.baseURL, voiceID), reqBody, queries...)
}

func (c *Client) GetModels() ([]Model, error) {
	body, err := c.doRequest(c.ctx, http.MethodGet, fmt.Sprintf("%s/models", c.baseURL), nil)
	if err != nil {
		return nil, err
	}

	var models []Model
	err = json.Unmarshal(body, &models)
	if err != nil {
		return nil, err
	}

	return models, nil
}

// func (c *Client) GetVoices() ([]Voice, error) {
// 	body, err := c.doRequest(c.ctx, "GET", fmt.Sprintf("%s/voices", c.baseURL), nil)
// 	if err != nil {
// 		return nil, err
// 	}

// 	var voiceResp VoicesResponse
// 	err = json.Unmarshal(body, &voiceResp)
// 	if err != nil {
// 		return nil, err
// 	}

// 	return voiceResp.Voices, nil
// }

// func (c *Client) GetDefaultVoiceSettings() (VoiceSettings, error) {
// 	var voiceSettings VoiceSettings
// 	body, err := c.doRequest(c.ctx, "GET", fmt.Sprintf("%s/voices/settings/default", c.baseURL), nil)
// 	if err != nil {
// 		return VoiceSettings{}, err
// 	}

// 	err = json.Unmarshal(body, &voiceSettings)
// 	if err != nil {
// 		return VoiceSettings{}, err
// 	}

// 	return voiceSettings, nil
// }

// func (c *Client) GetVoiceSettings(voiceId string) (VoiceSettings, error) {
// 	var voiceSettings VoiceSettings
// 	body, err := c.doRequest(c.ctx, "GET", fmt.Sprintf("%s/voices/%s/settings", c.baseURL, voiceId), nil)
// 	if err != nil {
// 		return VoiceSettings{}, err
// 	}

// 	err = json.Unmarshal(body, &voiceSettings)
// 	if err != nil {
// 		return VoiceSettings{}, err
// 	}

// 	return voiceSettings, nil
// }

// func (c *Client) GetVoice(voiceId string) (Voice, error) {
// 	var voice Voice
// 	body, err := c.doRequest(c.ctx, "GET", fmt.Sprintf("%s/voices/%s", c.baseURL, voiceId), nil)
// 	if err != nil {
// 		return Voice{}, err
// 	}

// 	err = json.Unmarshal(body, &voice)
// 	if err != nil {
// 		return Voice{}, err
// 	}

// 	return voice, nil
// }

// func (c *Client) DeleteVoice(voiceId string) error {
// 	_, err := c.doRequest(c.ctx, "GET", fmt.Sprintf("%s/voices/%s", c.baseURL, voiceId), nil)
// 	return err
// }