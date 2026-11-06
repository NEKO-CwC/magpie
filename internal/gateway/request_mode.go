package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

type requestModeKey struct{}

// withRequestMode fixes the failure policy for a request, including its
// translated calls, so a settings change only affects new requests.
func withRequestMode(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(requestModeKey{}).(bool); ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), requestModeKey{}, settings.Shared().ManagedRequests()))
}

// managedRequest leaves internal calls without a client request's policy
// on their existing behavior. Client requests acquire the policy at entry.
func managedRequest(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	managed, ok := ctx.Value(requestModeKey{}).(bool)
	return !ok || managed
}

func managedResponse(w http.ResponseWriter) bool {
	if h, ok := w.(*holdWriter); ok {
		return managedRequest(h.ctx)
	}
	return true
}

// translatedRequestError changes only the protocol's error envelope. A
// vendor's message and status do not acquire managed recovery advice.
func translatedRequestError(w http.ResponseWriter, proto provider.Protocol, res *http.Response, b []byte, u *Usage) (int, string) {
	for k, vs := range res.Header {
		if requestHopHeader(k, res.Header) || strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		w.Header()[k] = vs
	}
	u.ErrType = provider.ErrorType(b)
	msg := provider.APIError(b, res.Status)
	return writeVendorError(w, proto, res.StatusCode, msg, b), msg
}

// relayRequestError keeps a same-protocol HTTP error as the upstream sent
// it. Only a bounded prefix is read for the ledger; the rest still streams.
func relayRequestError(w http.ResponseWriter, res *http.Response, u *Usage) (int, string) {
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	u.RequestID, u.ErrType = requestID(res.Header), provider.ErrorType(b)
	msg := provider.APIError(b, res.Status)
	for k, vs := range res.Header {
		if requestHopHeader(k, res.Header) {
			continue
		}
		w.Header()[k] = vs
	}
	w.WriteHeader(res.StatusCode)
	if _, err := w.Write(b); err == nil {
		io.Copy(w, res.Body)
	}
	return res.StatusCode, msg
}

func requestHopHeader(key string, h http.Header) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	for _, line := range h.Values("Connection") {
		for _, k := range strings.Split(line, ",") {
			if strings.EqualFold(key, strings.TrimSpace(k)) {
				return true
			}
		}
	}
	return false
}
