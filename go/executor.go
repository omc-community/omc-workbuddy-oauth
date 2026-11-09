package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type credentialGate struct {
	cond   *sync.Cond
	active int
}

var gateMu sync.Mutex
var gates = map[string]*credentialGate{}

// acquire bounds simultaneous requests per credential, as WorkBuddy's plans
// expect.
func acquire(id string) func() {
	gateMu.Lock()
	g := gates[id]
	if g == nil {
		g = &credentialGate{cond: sync.NewCond(&gateMu)}
		gates[id] = g
	}
	for g.active >= getConfig().MaxConcurrency {
		g.cond.Wait()
	}
	g.active++
	gateMu.Unlock()
	return func() { gateMu.Lock(); g.active--; g.cond.Broadcast(); gateMu.Unlock() }
}

type requestPlan struct {
	HTTP       pluginapi.HTTPRequest
	Chat       map[string]any
	Messages   []any
	Model      workbuddyModel
	Credential credential
}

// prepareRequest turns an OpenAI chat completion into a signed WorkBuddy request.
func prepareRequest(req pluginapi.ExecutorRequest, sender httpSender) (requestPlan, error) {
	var plan requestPlan
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return plan, pluginabi.NewError("invalid_auth", "invalid WorkBuddy credential", 401)
	}
	spec, ok := configuredSpec(c.Site)
	if !ok {
		return plan, pluginabi.NewError("invalid_auth", "invalid WorkBuddy credential", 401)
	}
	model := trimModelPrefix(strings.TrimSpace(req.Model))
	if model == "" {
		return plan, pluginabi.NewError("invalid_request", "model is required", 400)
	}
	chat, err := parseChat(req.Payload)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", err.Error(), 400)
	}
	m, ok := findModel(sender, c, spec, model)
	if !ok {
		return plan, pluginabi.NewError("unknown_model", fmt.Sprintf("unknown WorkBuddy model %q", model), 400)
	}
	normalizeChat(chat, m)
	// WorkBuddy refuses a chat it cannot stream, so every request is streamed
	// upstream; a client that asked for one answer gets the stream read out. That
	// read needs the usage chunk, which WorkBuddy sends only when asked for it.
	chat["stream"] = true
	if !req.Stream {
		options, _ := chat["stream_options"].(map[string]any)
		if options == nil {
			options = map[string]any{}
		}
		options["include_usage"] = true
		chat["stream_options"] = options
	}
	chat["model"] = m.Key
	body, err := json.Marshal(chat)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", "invalid chat completion request", 400)
	}
	headers := http.Header{}
	signHeaders(spec, c, headers)
	chatHeaders(spec, headers)
	messages, _ := chat["messages"].([]any)
	attend(headers, messages, sessionOf(req.Headers), c)
	headers.Set("Content-Type", "application/json")
	return requestPlan{
		HTTP:       pluginapi.HTTPRequest{Method: "POST", URL: spec.Endpoint + chatPath, Headers: headers, Body: body},
		Chat:       chat,
		Messages:   messages,
		Model:      m,
		Credential: c,
	}, nil
}

// execute answers one non-streaming chat completion. WorkBuddy always streams, so
// the buffered body is read as SSE either way.
func execute(req pluginapi.ExecutorRequest, sender httpSender) (pluginapi.ExecutorResponse, error) {
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	release := acquire(req.AuthID)
	defer release()
	resp, err := sender.Do(plan.HTTP)
	if err != nil {
		return pluginapi.ExecutorResponse{}, transportFailure(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, classifyUpstream(plan.Credential, resp.StatusCode, resp.Headers, resp.Body)
	}
	if !bytes.Contains(resp.Body, []byte("data:")) {
		// A 200 that is not an event stream is WorkBuddy refusing in JSON.
		return pluginapi.ExecutorResponse{}, classifyUpstream(plan.Credential, http.StatusBadGateway, resp.Headers, resp.Body)
	}
	payload, err := aggregateStream(resp.Body, plan.Model.Key)
	if err != nil {
		var refusal *streamRefusal
		if errors.As(err, &refusal) {
			return pluginapi.ExecutorResponse{}, classifyUpstream(plan.Credential, 0, nil, refusal.Data)
		}
		return pluginapi.ExecutorResponse{}, pluginabi.NewError("upstream_error", err.Error(), 502)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": {"application/json"}},
		Metadata: map[string]any{
			"upstream_host":  plan.Credential.Site,
			"upstream_model": plan.Model.Key,
		},
	}, nil
}

type hostStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func readHostStream(id string) (hostReadResponse, error) {
	var r hostReadResponse
	if err := hostRPC("host.http.stream_read", map[string]any{"stream_id": id}, &r); err != nil {
		return r, &transportError{cause: err.Error()}
	}
	if r.Error != "" {
		return r, &transportError{cause: r.Error}
	}
	return r, nil
}

func closeHostStream(id string) {
	_ = hostRPC("host.http.stream_close", map[string]any{"stream_id": id}, nil)
}

func emitStreamChunk(id string, payload []byte) error {
	return hostRPC("host.stream.emit", map[string]any{"stream_id": id, "payload": payload}, nil)
}

func closeStream(id, message string) {
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message}, nil)
}

// closeStreamWithError ends a stream with the structured error the host's router
// reads: without error_detail the host sees a statusless plugin failure and
// answers every mid-stream refusal as a 500, so a 403 entitlement or a 429 rate
// limit never reaches the routing and cooldown decisions. An error that carries
// no pluginabi.Error is closed without one.
func closeStreamWithError(id, message string, err error) {
	var detail *pluginabi.Error
	_ = errors.As(err, &detail)
	if detail == nil {
		closeStream(id, message)
		return
	}
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message, "error_detail": detail}, nil)
}

// readWholeStream drains and closes a host HTTP stream, with a bound so a
// misbehaving upstream cannot exhaust memory.
func readWholeStream(id string) ([]byte, error) {
	defer closeHostStream(id)
	var out []byte
	for {
		r, err := readHostStream(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r.Payload...)
		if len(out) > 1<<20 {
			return nil, errors.New("upstream error body too large")
		}
		if r.Done {
			return out, nil
		}
	}
}

// Streaming seams. Tests replace them to exercise the pump without a plugin host.
var (
	readStreamChunk     = readHostStream
	emitStreamFrame     = emitStreamChunk
	openHTTPStream      = openUpstreamStream
	closeHostStreamWith = closeStreamWithError
	// pumpFinished is nil in production; a test sets it to learn that a pump has
	// returned before it puts the seams above back.
	pumpFinished func(source string)
)

func openUpstreamStream(plan requestPlan, callbackID string) (hostStreamResponse, error) {
	var response hostStreamResponse
	err := hostRPC("host.http.do_stream", map[string]any{"host_callback_id": callbackID, "request": plan.HTTP}, &response)
	return response, err
}

// streamPump copies one WorkBuddy event stream into the host stream. A frame
// is forwarded as its bare data payload: the host's OpenAI handler adds the
// "data: " prefix, the blank line and the [DONE] marker itself, and a host
// that forwards plugin chunks unchanged (older CPA does) would otherwise
// wrap the frame again and send the client "data: data: {...}", which its
// JSON parser refuses. An event with no data (a comment, a keep-alive) is
// not forwarded.
type streamPump struct {
	source     string
	target     string
	buffer     sseBuffer
	credential credential
	// headers are the response headers the stream opened with. A refusal inside
	// the stream has no headers of its own, so this is where its retry window is.
	headers http.Header
}

// pumpStart is what the pump found before the client was answered: the stream's
// first events, or the refusal that ended it.
type pumpStart struct {
	frames []sseFrame
	err    error
}

// firstFrameWindow is how long a streaming request waits for WorkBuddy's first
// event before it answers the client. WorkBuddy sends nothing while a reasoning
// model thinks, so waiting for the first event before replying held the client's
// headers — and every byte — for as long as the model thought; a caller, or a
// proxy along the way, then gave up. That is the HTTP 499 with "context canceled"
// a live streaming deepseek-v4.1-flash-sg request showed. Two seconds still
// catches a refusal WorkBuddy writes as the stream's first event.
var firstFrameWindow = 2 * time.Second

// first reads until the stream's first events arrive, tells the caller what it
// found, and returns it. A refusal in those first bytes keeps its real status.
func (p *streamPump) first(ready chan<- pumpStart) pumpStart {
	var frames []sseFrame
	for attempt := 0; attempt < 8 && len(frames) == 0; attempt++ {
		r, err := readStreamChunk(p.source)
		if err != nil {
			start := pumpStart{err: transportFailure(err)}
			ready <- start
			return start
		}
		frames = append(frames, p.buffer.push(r.Payload)...)
		if r.Done {
			frames = append(frames, p.buffer.flush()...)
			break
		}
	}
	for _, frame := range frames {
		if err := refusalOf(frame.Data); err != nil {
			start := pumpStart{err: classifyUpstream(p.credential, refusalStatus(err), p.headers, frame.Data)}
			ready <- start
			return start
		}
	}
	start := pumpStart{frames: frames}
	ready <- start
	return start
}

// deliver forwards complete events, and reports false when the client's stream is
// gone. The bare data payload is what the host accepts: it adds SSE framing itself.
func (p *streamPump) deliver(frames []sseFrame) bool {
	for _, frame := range frames {
		if isDone(frame.Data) || len(bytes.TrimSpace(frame.Data)) == 0 {
			continue
		}
		if emitStreamFrame(p.target, frame.Data) != nil {
			closeStream(p.target, "client stream closed")
			return false
		}
	}
	return true
}

func (p *streamPump) run() {
	for {
		r, err := readStreamChunk(p.source)
		if err != nil {
			closeHostStreamWith(p.target, streamFailureMessage(err), transportFailure(err))
			return
		}
		if !p.deliver(p.buffer.push(r.Payload)) {
			return
		}
		if r.Done {
			if !p.deliver(p.buffer.flush()) {
				return
			}
			// The host writes the terminal marker itself: its handler ends the
			// client's stream, and its translator tail turns the stream's end
			// into whatever the client's protocol closes with.
			closeStream(p.target, "")
			return
		}
	}
}

// executeStream opens the upstream stream, rejects a refusal before any chunk
// reaches the client, and then pumps asynchronously.
func executeStream(req pluginapi.ExecutorRequest, callbackID, streamID string, sender httpSender) (map[string]any, error) {
	if streamID == "" {
		return nil, pluginabi.NewError("invalid_request", "stream ID required", 400)
	}
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return nil, err
	}
	release := acquire(req.AuthID)
	response, err := openHTTPStream(plan, callbackID)
	if err != nil {
		release()
		return nil, transportFailure(err)
	}
	if response.StreamID == "" {
		release()
		return nil, pluginabi.NewError("transport_error", "WorkBuddy upstream stream unavailable", 503)
	}
	contentType := strings.ToLower(response.Headers.Get("Content-Type"))
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(contentType, "application/json") {
		raw, readErr := readWholeStream(response.StreamID)
		release()
		if readErr != nil {
			return nil, pluginabi.NewError("transport_error", "WorkBuddy upstream stream failed", 503)
		}
		return nil, classifyUpstream(plan.Credential, response.StatusCode, response.Headers, raw)
	}
	// Answer the client as soon as the stream is open. Reading until the answer
	// starts would keep the client waiting through a reasoning model's thinking,
	// and a refusal WorkBuddy writes as the stream's first event is still caught
	// inside firstFrameWindow.
	pump := &streamPump{source: response.StreamID, target: streamID, credential: plan.Credential, headers: response.Headers}
	started := make(chan bool, 1)
	ready := make(chan pumpStart, 1)
	go func() {
		defer release()
		defer closeHostStream(pump.source)
		defer func() {
			if pumpFinished != nil {
				pumpFinished(pump.source)
			}
		}()
		start := pump.first(ready)
		answered := <-started
		if start.err != nil {
			if answered {
				// The client has the stream; end it with the refusal instead of
				// leaving it open. The structured detail keeps the refusal's own
				// status for the host's routing.
				closeHostStreamWith(pump.target, start.err.Error(), start.err)
			}
			return
		}
		if !pump.deliver(start.frames) {
			return
		}
		pump.run()
	}()
	select {
	case start := <-ready:
		if start.err != nil {
			started <- false
			return nil, start.err
		}
		started <- true
	case <-time.After(firstFrameWindow):
		started <- true
	}
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

// streamFailureMessage says why a stream stopped, for the client's error frame.
func streamFailureMessage(err error) string {
	var e *pluginabi.Error
	if errors.As(transportFailure(err), &e) && e.HTTPStatus == 499 {
		return "the caller closed the request"
	}
	return "WorkBuddy stream failed"
}

// countTokens is not supported: WorkBuddy documents no token-count endpoint, and
// a guess would misbill clients that trust it.
func countTokens() (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, pluginabi.NewError("not_supported", "WorkBuddy does not provide a verified token-count endpoint", http.StatusNotImplemented)
}

// upstreamMessage reads WorkBuddy's refusal text from either shape it sends: its
// own {code, msg} envelope, or an OpenAI-style {error: {message}}.
func upstreamMessage(status int, body []byte) (code, message string) {
	env, _ := parseEnvelope(body)
	code = env.code()
	message = env.message()
	if message != "" {
		return code, message
	}
	var openai struct {
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &openai) == nil && openai.Error != nil {
		if strings.TrimSpace(openai.Error.Message) != "" {
			message = strings.TrimSpace(openai.Error.Message)
		}
		switch v := openai.Error.Code.(type) {
		case string:
			code = v
		case float64:
			code = fmt.Sprintf("%.0f", v)
		}
		if code == "" {
			code = openai.Error.Type
		}
		return code, message
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 400 {
		text = text[:400]
	}
	return "", text
}

// resetInstant is the moment a frequency limit says the allowance comes back.
// WorkBuddy writes it as a wall clock carrying its own offset, in either
// language: "your usage will reset at 2026-11-02 19:17:02 UTC+8" or
// "2026-11-09 21:01:11 UTC+8 重置". A wall clock without an offset is not read at
// all: the same digits mean sixteen different instants, and guessing one would
// rest a model for hours the account never asked for.
var resetInstant = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})\s*(?:UTC|GMT)\s*([+-])(\d{1,2})(?::?(\d{2}))?`)

// maxResetHint bounds what a message can ask for. WorkBuddy's windows run to a
// plan cycle, so a limit can legitimately sit weeks out; anything past a month
// is a misread date, and resting a model that long on a misread costs requests.
const maxResetHint = 31 * 24 * time.Hour

// retryHint is how long a limit still runs, in whole seconds, or 0 when nothing
// says. A Retry-After WorkBuddy sends is machine-stated and wins; otherwise the
// instant its message names is read.
func retryHint(headers http.Header, message string, now time.Time) int {
	if headers != nil {
		if value := strings.TrimSpace(headers.Get("Retry-After")); value != "" {
			if seconds, err := strconv.Atoi(value); err == nil {
				if seconds > 0 {
					return seconds
				}
				return 0
			}
			if at, err := http.ParseTime(value); err == nil {
				wait := at.Sub(now)
				if wait <= 0 {
					return 0
				}
				if seconds := int(wait / time.Second); seconds > 0 {
					return seconds
				}
				return 1
			}
		}
	}
	return resetHint(message, now)
}

// resetHint is how long a limit's message says it lasts, in whole seconds, or 0
// when the message names no instant still ahead of now.
//
// The number is not cosmetic: CPA reads pluginabi.Error.RetryAfterSeconds as the
// cooldown it applies (for a 429, to this model), and without it CPA falls back
// to its own quota backoff, which starts at one second and climbs. A limit that
// names its reset would then be retried long before the account is ready, and
// the console could only show CPA's guess instead of WorkBuddy's own deadline.
func resetHint(message string, now time.Time) int {
	m := resetInstant.FindStringSubmatch(message)
	if m == nil {
		return 0
	}
	hours, err := strconv.Atoi(m[8])
	if err != nil {
		return 0
	}
	minutes := 0
	if m[9] != "" {
		if minutes, err = strconv.Atoi(m[9]); err != nil {
			return 0
		}
	}
	offset := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
	if m[7] == "-" {
		offset = -offset
	}
	// ParseInLocation rejects a wall clock no zone can hold (month 13, minute
	// 61), which keeps a stray digit string from becoming a deadline.
	zone := time.FixedZone("", int(offset/time.Second))
	wall := m[1] + "-" + m[2] + "-" + m[3] + " " + m[4] + ":" + m[5] + ":" + m[6]
	at, err := time.ParseInLocation("2006-01-02 15:04:05", wall, zone)
	if err != nil {
		return 0
	}
	wait := at.Sub(now)
	if wait <= 0 || wait > maxResetHint {
		return 0
	}
	if seconds := int(wait / time.Second); seconds > 0 {
		return seconds
	}
	return 1
}

// classifyUpstream maps a WorkBuddy refusal onto the status the client should see.
// A lapsed sign-in must read as one so CPA can ask for a new one; a frequency
// limit keeps 429, its message, and the reset instant that message names, which
// CPA turns into the cooldown rather than guessing one.
func classifyUpstream(c credential, status int, headers http.Header, body []byte) error {
	site, ok := siteFor(c.Site)
	if !ok {
		site = siteSpec{Name: "WorkBuddy"}
	}
	code, message := upstreamMessage(status, body)
	if message == "" {
		message = http.StatusText(status)
	}
	if code != "" {
		switch code {
		case "10085":
			message = "WorkBuddy refused this client's identity (10085): " + message
		case "11128":
			message = "WorkBuddy refused an agent identity line in this prompt (11128): " + message
		}
	}
	// A plan that does not include a model says nothing about the request: the
	// model is hidden from this account's list, and the credential stays healthy.
	// The code is the contract, not the words: CPA reads "unknown_model" as this
	// credential not having the model, so the request falls through to another
	// credential or provider. A code it does not know would end the request.
	if model, plan, ok := refusedModel(message); ok {
		denyModel(c, model, plan)
		who := ""
		if plan != "" {
			who = " (" + plan + ")"
		}
		return pluginabi.NewError("unknown_model",
			fmt.Sprintf("%s: %s is not available for your plan%s; the model is hidden from this credential's list", site.Name, model, who),
			http.StatusBadRequest)
	}
	// The codes are more specific than the status: a refusal carried inside an OK
	// stream has no status at all, and an unapproved-channel refusal is 403
	// whatever the transport said.
	effective := status
	switch code {
	case "11128", "11153", "11158":
		effective = http.StatusForbidden
	case "14018":
		effective = http.StatusTooManyRequests
	case "10085":
		effective = http.StatusBadRequest
	}
	if effective == 0 {
		effective = http.StatusBadGateway
	}
	switch {
	case effective == http.StatusUnauthorized:
		return pluginabi.NewError("reauth_required", site.Name+" sign-in has expired; sign in again", http.StatusUnauthorized)
	case effective == http.StatusPaymentRequired:
		return pluginabi.NewError("payment_required", site.Name+": credits are spent: "+message, effective)
	case effective == http.StatusForbidden:
		return pluginabi.NewError("forbidden", site.Name+": "+message, effective)
	case effective == http.StatusTooManyRequests:
		// WorkBuddy's 14018 names an instant for this model and tells the user to
		// switch models, so the refusal stays model-scoped: marking the whole
		// credential would cool models the allowance still covers.
		err := pluginabi.NewError("rate_limited", site.Name+" is limiting this model: "+message, effective)
		err.RetryAfterSeconds = retryHint(headers, message, time.Now())
		return err
	case effective >= 500:
		return pluginabi.NewError("upstream_error", site.Name+" upstream failed: "+message, http.StatusBadGateway)
	case effective == http.StatusNotFound:
		return pluginabi.NewError("not_found", site.Name+": "+message, effective)
	case effective >= 300 && effective < 500:
		if strings.Contains(strings.ToLower(message), "prompt is too long") {
			message = "the prompt is longer than this model takes: " + message
		}
		return pluginabi.NewError("invalid_request", site.Name+": "+message, effective)
	default:
		return pluginabi.NewError("upstream_error", site.Name+": "+message, http.StatusBadGateway)
	}
}

// refusalStatus is the status a refusal inside a stream stands for: its own when
// it answered with one, else none, so the code decides.
func refusalStatus(err error) int {
	var refusal *wbError
	if errors.As(err, &refusal) {
		return refusal.Status
	}
	return 0
}
