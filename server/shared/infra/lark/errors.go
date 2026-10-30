// This file does one thing: make it easy for callers to tell what kind of error Feishu returned.
//
// Feishu errors come in two layers, told apart in completely different ways:
//
//  1. Transport layer: HTTP >= 300, collected into *APIError (status code + code + msg).
//  2. Business layer: HTTP is still 200, and the error is hidden in the response body. Things like
//     the device code flow's authorization_pending belong to this layer; see PollStatus.
//
// Mixing up the two layers leads to bugs: retrying "waiting for user authorization" as a failed
// request, or the other way round, polling forever on a real network failure as if it were "not
// authorized yet".

package lark

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Errors this package produces itself (the request was not sent yet, or the response is missing
// required fields).
var (
	// ErrInvalidConfig means the config is invalid, for example a misspelled region name.
	ErrInvalidConfig = errors.New("lark: 配置不合法")
	// ErrInvalidArgument means a call argument is invalid, for example an empty device_code.
	ErrInvalidArgument = errors.New("lark: 参数不合法")
	// ErrInvalidResponse means Feishu's response is missing a required field, for example begin
	// gave no device_code.
	ErrInvalidResponse = errors.New("lark: 响应不合法")
)

// APIError is an HTTP error returned by Feishu.
//
// It is only produced for HTTP >= 300. Business-layer errors (HTTP 200 with an error in the body)
// do not go through here.
type APIError struct {
	// HTTPStatusCode is the HTTP status code.
	HTTPStatusCode int
	// Code is Feishu's business error code, taken from the code field of the response body.
	Code string
	// Message is a readable error description, taken from msg / error_description.
	Message string
	// LogID is the most useful thing to give the Feishu side when reporting a problem.
	LogID string
	// RawBody is the unparsed response body; look at it when the fields above cannot be parsed.
	RawBody string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString("飞书返回 HTTP ")
	b.WriteString(strconv.Itoa(e.HTTPStatusCode))
	if text := http.StatusText(e.HTTPStatusCode); text != "" {
		b.WriteString(" ")
		b.WriteString(text)
	}
	if e.Code != "" {
		b.WriteString(" code=")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(" message=")
		b.WriteString(e.Message)
	}
	if e.LogID != "" {
		b.WriteString(" log_id=")
		b.WriteString(e.LogID)
	}
	if e.Code == "" && e.Message == "" && e.RawBody != "" {
		b.WriteString(" body=")
		b.WriteString(truncate(e.RawBody, 256))
	}
	return b.String()
}

// newAPIError turns a failed response into a structured error.
func newAPIError(status int, raw []byte) *APIError {
	err := &APIError{HTTPStatusCode: status, RawBody: string(raw)}
	var body struct {
		Code             json.Number `json:"code"`
		Msg              string      `json:"msg"`
		Error            string      `json:"error"`
		ErrorDescription string      `json:"error_description"`
		LogID            string      `json:"log_id"`
	}
	if json.Unmarshal(raw, &body) == nil {
		err.Code = body.Code.String()
		if err.Code == "" {
			err.Code = body.Error
		}
		err.Message = body.Msg
		if err.Message == "" {
			err.Message = body.ErrorDescription
		}
		err.LogID = body.LogID
	}
	return err
}

// AsAPIError tries to turn err back into Feishu's structured error.
//
//	if apiErr, ok := lark.AsAPIError(err); ok {
//	    log.Printf("Feishu error code=%s log_id=%s", apiErr.Code, apiErr.LogID)
//	}
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// StatusCode returns the HTTP status code of the error; 0 when it is not a Feishu structured error.
func StatusCode(err error) int {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.HTTPStatusCode
	}
	return 0
}

// LogID returns the log_id carried by the error, for the Feishu side when reporting a problem.
func LogID(err error) string {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.LogID
	}
	return ""
}

// IsAuthError reports whether it is 401/403, i.e. invalid credentials or no permission.
//
// The inspection endpoints land here when the app has not been granted
// application:application:self_manage, and that is **expected**: it only means this app is not
// allowed to inspect its own config, not that the caller made a mistake.
func IsAuthError(err error) bool {
	switch StatusCode(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	default:
		return false
	}
}

// IsRateLimited reports whether it is 429, for use with backoff retries.
func IsRateLimited(err error) bool {
	return StatusCode(err) == http.StatusTooManyRequests
}
