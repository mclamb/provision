// Package api implements a client API for working with
// digitalrebar/provision.
package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"golang.org/x/net/http2"

	"github.com/VictorLowther/jsonpatch2"

	"github.com/digitalrebar/logger"
	"github.com/digitalrebar/provision/v4/models"
)

var (
	defaultLogBuf = logger.New(log.New(os.Stderr, "", 0))
)

// APIPATH is the base path for all API endpoints that digitalrebar
// provision provides.
const APIPATH = "/api/v3"

// Client wraps *http.Client to include our authentication routines
// and routines for handling some of the boilerplate CRUD operations
// against digitalrebar provision.
type Client struct {
	logger.Logger
	*http.Client
	// The amount of time to allow any single round trip by default.
	// Defaults to no timeout.  This will be overridden by a Client.Req().Context()
	RoundTripTimeout             time.Duration
	mux                          *sync.Mutex
	endpoint, username, password string
	neverProxy                   bool
	token                        *models.UserToken
	closer                       chan struct{}
	closed                       bool
	traceLvl                     string
	traceToken                   string
	info                         *models.Info
	iMux                         *sync.Mutex
	urlProxy                     string
}

func (c *Client) realEndpoint() string {
	if locallyProxied(c.neverProxy) == "" {
		return c.endpoint
	}
	return "http://unix"
}

// Endpoint returns the address of the dr-provision API endpoint that
// we are talking to.
func (c *Client) Endpoint() string {
	return c.endpoint
}

// Username returns the username that the Client is using.  If the
// client was created via TokenSession, then this will return an empty
// string.
func (c *Client) Username() string {
	return c.username
}

func (c *Client) UrlForProxy(proxy string, args ...string) (*url.URL, error) {
	if proxy == "" {
		return url.ParseRequestURI(c.realEndpoint() + path.Join(APIPATH, path.Join(args...)))
	}
	return url.ParseRequestURI(c.realEndpoint() + "/" + proxy + path.Join(APIPATH, path.Join(args...)))
}

func (c *Client) UrlFor(args ...string) (*url.URL, error) {
	return c.UrlForProxy("", args...)
}

// Trace sets the log level that incoming requests generated by a
// Client will be logged at, overriding the levels they would normally
// be logged at on the server side.  Setting lvl to an empty string
// turns off tracing.
func (c *Client) Trace(lvl string) {
	c.mux.Lock()
	defer c.mux.Unlock()
	c.traceLvl = lvl
}

// TraceToken is a unique token that the server-side logger will emit
// a log for at the Error log level. It can be used to tie logs
// generated on the server side to requests made by a specific Client.
func (c *Client) TraceToken(t string) {
	c.mux.Lock()
	defer c.mux.Unlock()
	c.traceToken = t
}

// File initiates a download from the static file service on the
// dr-provision endpoint.  It is up to the caller to ensure that the
// returned ReadCloser gets closed, otherwise stale HTTP connections
// will leak.
func (c *Client) File(pathParts ...string) (io.ReadCloser, error) {
	info, err := c.Info()
	if err != nil {
		return nil, err
	}
	if info.FilePort == 0 {
		return nil, fmt.Errorf("Static file service not running")
	}
	url := fmt.Sprintf("http://%s:%d/%s", info.Address, info.FilePort, path.Join(pathParts...))
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// R encapsulates a single Request/Response round trip.  It has a slew
// of helper methods that can be chained together to handle all common
// operations with this API.  It handles capturing any errors that may
// occur in building and executing the request.
type R struct {
	c                    *Client
	method               string
	uri                  *url.URL
	header               http.Header
	body                 io.Reader
	Req                  *http.Request
	Resp                 *http.Response
	err                  *models.Error
	paranoid             bool
	noRetry              bool
	traceLvl, traceToken string
	proxy                string
	params               url.Values
	ctx                  context.Context
	eTag                 string
}

// Req creates a new R for the current client.
// It defaults to using the GET method.
func (c *Client) Req() *R {
	c.mux.Lock()
	defer c.mux.Unlock()
	res := &R{
		c:          c,
		traceLvl:   c.traceLvl,
		traceToken: c.traceToken,
		method:     "GET",
		header:     http.Header{},
		proxy:      c.urlProxy,
		params:     url.Values{},
		ctx:        context.Background(),
		err: &models.Error{
			Type: "CLIENT_ERROR",
		},
	}
	return res
}

// Context will set the Context.Context that should be used for processing this
// specific request.  If you set a context, the usual retry logic will
// be disabled as if you had also called FailFast
func (r *R) Context(c context.Context) *R {
	r.ctx = c
	r.noRetry = true
	return r
}

// Proxy will insert the string between the host and the api/v3 path in the url.
func (r *R) Proxy(proxy string) *R {
	r.proxy = proxy
	return r
}

// Trace will arrange for the server to log this specific request at
// the passed-in Level, overriding any client Trace requests or the
// levels things would usually be logged at by the server.
func (r *R) Trace(lvl string) *R {
	r.traceLvl = lvl
	return r
}

// TraceToken is a unique token that the server-side logger will emit
// a log for at the Error log level. It can be used to tie logs
// generated on the server side to requests made by a specific Req.
func (r *R) TraceToken(t string) *R {
	r.traceToken = t
	return r
}

// Meth sets an arbitrary method for R
func (r *R) Meth(v string) *R {
	r.method = v
	return r
}

// Get sets the R method to GET
func (r *R) Get() *R {
	return r.Meth("GET")
}

func (r *R) List(prefix string) *R {
	return r.Get().UrlFor(prefix)
}

// Del sets the R method to DELETE
func (r *R) Del() *R {
	return r.Meth("DELETE")
}

// Head sets the R method to HEAD
func (r *R) Head() *R {
	return r.Meth("HEAD")
}

// Put sets the R method to PUT, and arranges for b to be used as the
// body of the request by calling r.Body(). If no body is desired, b
// can be nil
func (r *R) Put(b interface{}) *R {
	return r.Meth("PUT").Body(b)
}

// Patch sets the R method to PATCH, and arranges for b (which must be
// a valid JSON patch) to be used as the body of the request by
// calling r.Body().
func (r *R) Patch(b jsonpatch2.Patch) *R {
	return r.Meth("PATCH").Body(b)
}

// Must be used before PatchXXX calls
func (r *R) ParanoidPatch() *R {
	r.paranoid = true
	return r
}

// Wait adds wait time for long polling - for HEAD and GET calls
func (r *R) Wait(etag, dur string) *R {
	if r.header == nil {
		r.header = http.Header{}
	}
	r.header.Add("If-None-Match", etag)
	r.header.Add("Prefer", fmt.Sprintf("wait=%s", dur))
	return r
}

// PatchObj generates a PATCH request for the differences between old and new.
func (r *R) PatchObj(old, new interface{}) *R {
	b, err := GenPatch(old, new, r.paranoid)
	if err != nil {
		r.err.AddError(err)
		return r
	}
	return r.Meth("PATCH").Body(b)
}

// PatchTo generates a Patch request that will transform old into new.
func (r *R) PatchTo(old, new models.Model) *R {
	if old.Prefix() != new.Prefix() || old.Key() != new.Key() {
		r.err.Model = old.Prefix()
		r.err.Key = old.Key()
		r.err.Errorf("Cannot patch from %T to %T, or change keys from %s to %s", old, new, old.Key(), new.Key())
		return r
	}
	return r.PatchObj(old, new).UrlForM(old)
}

func (r *R) PatchToFull(old models.Model, new models.Model, paranoid bool) (models.Model, error) {
	res := models.Clone(old)
	if paranoid {
		r = r.ParanoidPatch()
	}
	err := r.PatchTo(old, new).Do(&res)
	if err != nil {
		return old, err
	}
	return res, err
}

// Fill fills in m with the corresponding data from the dr-provision
// server.
func (r *R) Fill(m models.Model) error {
	r.err.Model = m.Prefix()
	r.err.Model = m.Key()
	if m.Key() == "" {
		r.err.Errorf("Cannot Fill %s with an empty key", m.Prefix())
		return r.err
	}
	return r.Get().UrlForM(m).Do(&m)
}

// Post sets the R method to POST, and arranges for b to be the body
// of the request by calling r.Body().
func (r *R) Post(b interface{}) *R {
	return r.Meth("POST").Body(b)
}

// Delete deletes a single object.
func (r *R) Delete(m models.Model) error {
	r.err.Model = m.Prefix()
	r.err.Model = m.Key()
	if m.Key() == "" {
		r.err.Errorf("Cannot Delete %s with an empty key", m.Prefix())
		return r.err
	}
	return r.Del().UrlForM(m).Do(&m)
}

// UrlFor arranges for a sane request URL to be used for R.
// The generated URL will be in the form of:
//
//    /api/v3/path.Join(args...)
func (r *R) UrlFor(args ...string) *R {
	res, err := r.c.UrlForProxy(r.proxy, args...)
	if err != nil {
		r.err.AddError(err)
		return r
	}
	r.uri = res
	return r
}

// UrlForM is similar to UrlFor, but the prefix and key of the
// passed-in Model will be used as the first two path components in
// the URL after /api/v3.  If m.Key() == "", it will be omitted.
func (r *R) UrlForM(m models.Model, rest ...string) *R {
	r.err.Model = m.Prefix()
	r.err.Key = m.Key()
	args := []string{m.Prefix(), m.Key()}
	args = append(args, rest...)
	return r.UrlFor(args...)
}

// Params appends query parameters to the URL R will use.
// You must pass an  even number of parameters to Params
func (r *R) Params(args ...string) *R {
	if len(args)&1 == 1 {
		r.err.Errorf("Params was not passed an even number of arguments")
		return r
	}
	for i := 1; i < len(args); i += 2 {
		r.params.Set(args[i-1], args[i])
	}
	return r
}

// Filter is a helper for using freeform index operations.
// The prefix arg is the type of object you want to filter, and filterArgs
// describes how you want the results filtered.  Currently, filterArgs must be
// in the following format:
//
//    "slim" "Meta|Params|Meta,Params"
//        to reduce the amount of data sent back
//    "params" "p1,p2,p3"
//        to reduce the returned parameters to the specified set.
//    "reverse"
//        to reverse the order of the results
//    "sort" "indexName"
//        to sort the results according to indexName's native ordering
//    "limit" "number"
//        to limit the number of results returned
//    "offset" "number"
//        to skip <number> of results before returning
//    "indexName" "Eq/Lt/Lte/Gt/Gte/Ne" "value"
//        to return results Equal, Less Than, Less Than Or Equal, Greater Than, Greater Than Or Equal, or Not Equal to value according to IndexName
//    "indexName" "Re" "re2 compatible regular expression"
//        to return results where values in indexName match the passed-in regular expression.
//        The index must have the Regex flag equal to True
//    "indexName" "Between/Except" "lowerBound" "upperBound"
//        to return values Between(inclusive) lowerBound and Upperbound or its complement for Except.
//    "indexName" "In/Nin" "comma,separated,list,of,values"
//        to return values either in the list of values or not in the listr of values
//
// If formatArgs does not contain some valid combination of the above, the request will fail.
func (r *R) Filter(prefix string, filterArgs ...string) *R {
	r.Get().UrlFor(prefix)
	finalParams := []string{}
	i := 0
	for i < len(filterArgs) {
		filter := filterArgs[i]
		switch filter {
		case "reverse", "decode":
			finalParams = append(finalParams, filter, "true")
			i++
		case "sort", "limit", "offset", "slim", "params":
			if len(filterArgs)-i < 2 {
				r.err.Errorf("Invalid Filter: %s requires exactly one parameter", filter)
				return r
			}
			finalParams = append(finalParams, filter, filterArgs[i+1])
			i += 2
		default:
			if len(filterArgs)-i < 2 {
				r.err.Errorf("Invalid Filter: %s requires an op and at least 1 parameter", filter)
				return r
			}
			op := strings.Title(strings.ToLower(filterArgs[i+1]))
			i += 2
			switch op {
			case "Eq", "Lt", "Lte", "Gt", "Gte", "Ne", "In", "Nin", "Re":
				if len(filterArgs)-i < 1 {
					r.err.Errorf("Invalid Filter: %s op %s requires 1 parameter", filter, op)
					return r
				}
				finalParams = append(finalParams, filter, fmt.Sprintf("%s(%s)", op, filterArgs[i]))
				i++
			case "Between", "Except":
				if len(filterArgs)-i < 1 || (len(filterArgs)-i < 2 && !strings.Contains(filterArgs[i], ",")) {
					r.err.Errorf("Invalid Filter: %s op %s requires 1 or 2 parameters", filter, op)
					return r
				}
				if !strings.Contains(filterArgs[i], ",") {
					finalParams = append(finalParams, filter, fmt.Sprintf("%s(%s,%s)", op, filterArgs[i], filterArgs[i+1]))
					i += 2
				} else {
					finalParams = append(finalParams, filter, fmt.Sprintf("%s(%s)", op, filterArgs[i]))
					i += 1
				}
			default:
				r.err.Errorf("Invalid Filter %s: unknown op %s", filter, op)
				return r
			}
		}
	}
	return r.Params(finalParams...)
}

// Headers arranges for its arguments to be added as HTTP headers.
// You must pass an even number of arguments to Headers
func (r *R) Headers(args ...string) *R {
	if len(args)&1 == 1 {
		r.err.Errorf("WithHeaders was not passed an even number of arguments")
		return r
	}
	if r.header == nil {
		r.header = http.Header{}
	}
	for i := 1; i < len(args); i += 2 {
		r.header.Add(args[i-1], args[i])
	}
	return r
}

// Body arranges for b to be used as the body of the request.
// It also sets the Content-Type of the request depending on what the body is:
//
// If b is an io.Reader or a raw byte array, Content-Type will be set to application/octet-stream,
// otherwise Content-Type will be set to application/json.
//
// If b is something other than nil, an io.Reader, or a byte array,
// Body will attempt to marshal the object as a JSON byte array and
// use that.
func (r *R) Body(b interface{}) *R {
	switch obj := b.(type) {
	case nil:
		r.Headers("Content-Type", "application/json")
	case io.Reader:
		r.Headers("Content-Type", "application/octet-stream")
		r.body = obj
	case []byte:
		r.Headers("Content-Type", "application/octet-stream")
		r.body = bytes.NewReader(obj)
	default:
		r.Headers("Content-Type", "application/json")
		buf, err := json.Marshal(&obj)
		if err != nil {
			r.err.AddError(err)
		} else {
			r.body = bytes.NewReader(buf)
		}
	}
	return r
}

// FailFast skips the usual fibbonaci backoff retry in the case of
// transient errors.
func (r *R) FailFast() *R {
	r.noRetry = true
	return r
}

// Response executes the request and returns a raw http.Response.
// The caller must close the response body when finished with it.
func (r *R) Response() (*http.Response, error) {
	if r.uri == nil {
		r.err.Errorf("No URL to talk to")
		return nil, r.err
	}
	if r.c.RoundTripTimeout > 0 && r.ctx == context.Background() {
		r.noRetry = true
		var cancel context.CancelFunc
		r.ctx, cancel = context.WithDeadline(r.ctx, time.Now().Add(r.c.RoundTripTimeout))
		defer cancel()
	}
	r.uri.RawQuery = r.params.Encode()
	r.c.mux.Lock()
	if r.c.closed {
		r.c.mux.Unlock()
		r.err.Errorf("Connection Closed")
		return nil, r.err
	}
	r.c.mux.Unlock()
	if r.err.ContainsError() {
		return nil, r.err
	}
	r.Headers("Cache-Control", "no-store")
	if r.traceLvl != "" {
		r.Headers("X-Log-Request", r.traceLvl)
		r.Headers("X-Log-Token", r.traceToken)
	}
	timeouts := []time.Duration{
		time.Second,
		time.Second,
		2 * time.Second,
		3 * time.Second,
		5 * time.Second,
		8 * time.Second,
	}
	var resp *http.Response
	var err error
	for _, waitFor := range timeouts {
		var req *http.Request
		req, err = http.NewRequestWithContext(r.ctx, r.method, r.uri.String(), r.body)
		if err != nil {
			r.err.AddError(err)
			return nil, r.err
		}
		req.Header = r.header
		r.Req = req
		r.c.Authorize(req)
		resp, err = r.c.Do(req)
		if err == nil || r.noRetry {
			break
		}
		if r.body == nil {
			r.c.mux.Lock()
			if r.c.closed {
				r.c.mux.Unlock()
				r.err.Errorf("Connection Closed")
				return nil, r.err
			}
			r.c.mux.Unlock()
			r.c.iMux.Lock()
			r.c.info = nil
			r.c.iMux.Unlock()
			time.Sleep(waitFor)
			continue
		}
		seeker, ok := r.body.(io.ReadSeeker)
		if r.body != nil && !ok {
			// we cannot rewind the body, so don't even try.
			break
		}
		if i, err := seeker.Seek(0, io.SeekStart); err != nil || i != 0 {
			break
		}
		r.c.mux.Lock()
		if r.c.closed {
			r.c.mux.Unlock()
			r.err.Errorf("Connection Closed")
			return nil, r.err
		}
		r.c.mux.Unlock()
		time.Sleep(waitFor)
	}
	if err != nil {
		r.err.AddError(err)
		return nil, r.err
	}
	r.Resp = resp
	r.eTag = resp.Header.Get("ETag")
	return r.Resp, r.err.HasError()
}

// Do attempts to execute the reqest built up by previous method calls
// on R.  If any errors occurred while building up the request, they
// will be returned and no API interaction will actually take place.
// Otherwise, Do will generate an http.Request, perform it, and
// marshal the results to val.  If any errors occur while processing
// the request, fibonacci based backoff will be performed up to 6
// times.
//
// If val is an io.Writer, the body of the response will be copied
// verbatim into val using io.Copy
//
// Otherwise, the response body will be unmarshalled into val as
// directed by the Content-Type header of the response.
func (r *R) Do(val interface{}) error {
	r.Headers("Accept", "application/json")
	switch val.(type) {
	case io.Writer:
		r.Headers("Accept", "application/octet-stream")
	}
	resp, err := r.Response()
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		if r.method == "HEAD" {
			r.err.Errorf(http.StatusText(resp.StatusCode))
			r.err.Code = resp.StatusCode
			return r.err
		}
		buf, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		res := &models.Error{}
		if err := json.Unmarshal(buf, res); err != nil {
			r.err.Code = resp.StatusCode
			r.err.AddError(err)
			r.err.Errorf("Raw response: %s", string(buf))
			return r.err
		}
		return res
	}
	r.eTag = resp.Header.Get("ETag")
	if wr, ok := val.(io.Writer); ok {
		if resp.StatusCode == 304 {
			// Pretend we wrote the whole thing.
			if fi, ok := wr.(io.WriteSeeker); ok {
				fi.Seek(0, io.SeekEnd)
			}
			return nil
		}
		if resp.StatusCode < 300 {
			_, err := io.Copy(wr, resp.Body)
			r.err.AddError(err)
			return r.err.HasError()
		}
	}
	if r.method == "HEAD" {
		if resp.StatusCode <= 300 || resp.StatusCode == 304 {
			if val != nil {
				return models.Remarshal(resp.Header, val)
			}
			return nil
		}
		r.err.Errorf(http.StatusText(resp.StatusCode))
		r.err.Code = resp.StatusCode
		return r.err
	}
	if resp.StatusCode == 304 {
		return nil
	}
	var dec Decoder
	ct := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	switch mt {
	case "application/json":
		dec = json.NewDecoder(resp.Body)
	default:
		r.err.Errorf("Cannot handle content-type %s", ct)
		dump, _ := httputil.DumpResponse(resp, true)
		r.err.Errorf("Resp: \n%s", string(dump))
	}
	if dec == nil {
		r.err.Errorf("No decoder for content-type %s", ct)
		return r.err
	}
	if val != nil && resp.Body != nil && resp.ContentLength != 0 {
		r.err.AddError(dec.Decode(val))
	}
	if f, ok := val.(models.Filler); ok && err != nil {
		f.Fill()
	}
	return r.err.HasError()
}

func (r *R) GetETag() string {
	return r.eTag
}

// Close should be called whenever you no longer want to use this
// client connection.  It will stop any token refresh routines running
// in the background, and force any API calls made to this client that
// would communicate with the server to return an error
func (c *Client) Close() {
	close(c.closer)
	c.mux.Lock()
	c.closed = true
	c.mux.Unlock()
	c.iMux.Lock()
	c.info = nil
	c.iMux.Unlock()
}

// Token returns the current authentication token associated with the
// Client.
func (c *Client) Token() string {
	c.mux.Lock()
	defer c.mux.Unlock()
	if c.token == nil {
		return ""
	}
	return c.token.Token
}

// Info returns some basic system information that was retrieved as
// part of the initial authentication.
func (c *Client) Info() (*models.Info, error) {
	c.iMux.Lock()
	if c.info != nil {
		c.iMux.Unlock()
		return c.info, nil
	}
	c.info = &models.Info{}
	c.iMux.Unlock()
	return c.info, c.Req().UrlFor("info").Do(c.info)
}

// Objects returns a list of objects in the DRP system
func (c *Client) Objects() ([]string, error) {
	objects := []string{}
	return objects, c.Req().UrlFor("objects").Do(&objects)
}

// Logs returns the currently buffered logs from the dr-provision server
func (c *Client) Logs() ([]logger.Line, error) {
	res := []logger.Line{}
	return res, c.Req().UrlFor("logs").Do(&res)
}

// Authorize sets the Authorization header in the Request with the
// current bearer token.  The rest of the helper methods call this, so
// you don't have to unless you are building your own http.Requests.
func (c *Client) Authorize(req *http.Request) error {
	if req.Header.Get("Authorization") == "" {
		// If we have a token use it, otherwise basic auth
		if c.Token() != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token())
		} else {
			basicAuth := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
			req.Header.Set("Authorization", "Basic "+basicAuth)
		}
	}
	return nil
}

// ListBlobs lists the names of all the binary objects at 'at', using
// the indexing parameters suppied by params.
func (c *Client) ListBlobs(at string, params ...string) ([]string, error) {
	res := []string{}
	return res, c.Req().UrlFor(path.Join("/", at)).Params(params...).Do(&res)
}

// GetBlob fetches a binary blob from the server, writing it to the
// passed io.Writer.  If the io.Writer is actually an io.ReadWriteSeeker
// with a Truncate method, GetBlob will only download the file
// if it has changed on the server side.
func (c *Client) GetBlob(dest io.Writer, at ...string) error {
	req := c.Req().UrlFor(path.Join("/", path.Join(at...)))
	shasum := ""
	var startSz int64
	if fi, ok := dest.(*os.File); ok {
		st, err := fi.Stat()
		if err == nil {
			startSz = st.Size()
		}
		mts := &models.ModTimeSha{}
		if _, err := mts.Regenerate(fi); err != nil {
			shasum = mts.String()
		}
	}
	if shasum != "" {
		req.Headers("If-None-Match", `"SHA256:`+shasum+`"`)
	}
	if err := req.Do(dest); err != nil {
		return err
	}
	if fi, ok := dest.(*os.File); ok {
		sz, _ := fi.Seek(0, io.SeekCurrent)
		if sz != startSz {
			if err := fi.Truncate(sz); err != nil {
				return err
			}
		}
		mts := &models.ModTimeSha{}
		mts.Regenerate(fi)
	}
	return nil
}

// GetBlobSum fetches the checksum for the blob
func (c *Client) GetBlobSum(at ...string) (string, error) {
	h := http.Header{}
	err := c.Req().Head().UrlFor(path.Join("/", path.Join(at...))).Do(&h)
	if err != nil {
		return "", err
	}
	sum := h.Get("X-DRP-SHA256SUM")
	if sum == "" {
		sum = strings.Trim(h.Get("ETag"), `"`)
		if parts := strings.SplitN(sum, ":", 2); len(parts) == 2 {
			sum = parts[1]
		}

	}
	return sum, nil
}

// PostBlobExplode uploads the binary blob contained in the passed io.Reader
// to the location specified by at on the server.  You are responsible
// for closing the passed io.Reader.  Sends the explode boolean as a query parameter.
func (c *Client) PostBlobExplode(blob io.Reader, explode bool, at ...string) (models.BlobInfo, error) {
	res := models.BlobInfo{}
	r := c.Req().Post(blob).UrlFor(path.Join("/", path.Join(at...)))
	if explode {
		r = r.Params("explode", "true")
	}
	return res, r.Do(&res)
}

// PostBlob uploads the binary blob contained in the passed io.Reader
// to the location specified by at on the server.  You are responsible
// for closing the passed io.Reader.
func (c *Client) PostBlob(blob io.Reader, at ...string) (models.BlobInfo, error) {
	return c.PostBlobExplode(blob, false, at...)
}

// DeleteBlob deletes a blob on the server at the location indicated
// by 'at'
func (c *Client) DeleteBlob(at ...string) error {
	return c.Req().Del().UrlFor(path.Join("/", path.Join(at...))).Do(nil)
}

// AllIndexes returns all the static indexes available for all object
// types on the server.
func (c *Client) AllIndexes() (map[string]map[string]models.Index, error) {
	res := map[string]map[string]models.Index{}
	return res, c.Req().UrlFor("indexes").Do(&res)
}

// Indexes returns all the static indexes available for a given type
// of object on the server.
func (c *Client) Indexes(prefix string) (map[string]models.Index, error) {
	res := map[string]models.Index{}
	return res, c.Req().UrlFor("indexes", prefix).Do(&res)
}

// OneIndex tests to see if there is an index on the object type
// indicated by prefix for a specific parameter.  If the returned
// Index is empty, there is no such Index.
func (c *Client) OneIndex(prefix, param string) (models.Index, error) {
	res := models.Index{}
	return res, c.Req().UrlFor("indexes", prefix, param).Do(&res)
}

// ListModel returns a list of models for prefix matching the request
// parameters passed in by params.
func (c *Client) ListModel(prefix string, params ...string) ([]models.Model, error) {
	ref, err := models.New(prefix)
	if err != nil {
		return nil, err
	}
	res := ref.SliceOf()
	err = c.Req().UrlForM(ref).Params(params...).Do(&res)
	if err != nil {
		return nil, err
	}
	return ref.ToModels(res), nil
}

// GetModel returns an object if type prefix with the unique
// identifier key, if such an object exists.  Key can be either the
// unique key for an object, or any field on an object that has an
// index that enforces uniqueness.
func (c *Client) GetModel(prefix, key string, params ...string) (models.Model, error) {
	res, err := models.New(prefix)
	if err != nil {
		return nil, err
	}
	return res, c.Req().UrlFor(res.Prefix(), key).Params(params...).Do(res)
}

func (c *Client) GetModelForPatch(prefix, key string, params ...string) (models.Model, models.Model, error) {
	ref, err := c.GetModel(prefix, key, params...)
	if err != nil {
		return nil, nil, err
	}
	return ref, models.Clone(ref), nil
}

// ExistsModel tests to see if an object exists on the server
// following the same rules as GetModel
func (c *Client) ExistsModel(prefix, key string) (bool, error) {
	err := c.Req().Head().UrlFor(prefix, key).Do(nil)
	if e, ok := err.(*models.Error); ok && e.Code == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// FillModel fills the passed-in model with new information retrieved
// from the server.
func (c *Client) FillModel(ref models.Model, key string) error {
	err := c.Req().UrlFor(ref.Prefix(), key).Do(&ref)
	return err
}

// CreateModel takes the passed-in model and creates an instance of it
// on the server.  It will return an error if the passed-in model does
// not validate or if it already exists on the server.
func (c *Client) CreateModel(ref models.Model) error {
	err := c.Req().Post(ref).UrlFor(ref.Prefix()).Do(&ref)
	return err
}

// DeleteModel deletes the model matching the passed-in prefix and
// key.  It returns the object that was deleted.
func (c *Client) DeleteModel(prefix, key string) (models.Model, error) {
	res, err := models.New(prefix)
	if err != nil {
		return nil, err
	}
	return res, c.Req().Del().UrlFor(prefix, key).Do(&res)
}

func (c *Client) reauth(tok *models.UserToken) error {
	return c.Req().UrlFor("users", c.username, "token").Params("ttl", "600").Do(&tok)
}

// PatchModel attempts to update the object matching the passed prefix
// and key on the server side with the passed-in JSON patch (as
// sepcified in https://tools.ietf.org/html/rfc6902).  To ensure that
// conflicting changes are rejected, your patch should contain the
// appropriate test stanzas, which will allow the server to detect and
// reject conflicting changes from different sources.
func (c *Client) PatchModel(prefix, key string, patch jsonpatch2.Patch) (models.Model, error) {
	new, err := models.New(prefix)
	if err != nil {
		return nil, err
	}
	err = c.Req().Patch(patch).UrlFor(prefix, key).Do(&new)
	return new, err
}

func (c *Client) PatchTo(old models.Model, new models.Model) (models.Model, error) {
	return c.PatchToFull(old, new, false)
}

func (c *Client) PatchToFull(old models.Model, new models.Model, paranoid bool) (models.Model, error) {
	return c.Req().PatchToFull(old, new, paranoid)
}

// PutModel replaces the server-side object matching the passed-in
// object with the passed-in object.  Note that PutModel does not
// allow the server to detect and reject conflicting changes from
// multiple sources.
func (c *Client) PutModel(obj models.Model) error {
	return c.Req().Put(obj).UrlForM(obj).Do(&obj)
}

func (c *Client) Websocket(at string) (*websocket.Conn, error) {
	subpath := path.Join(APIPATH, at)
	c.mux.Lock()
	if c.urlProxy != "" {
		subpath = path.Join(subpath, c.urlProxy)
	}
	c.mux.Unlock()
	ep, err := url.ParseRequestURI(c.endpoint + subpath)
	if err != nil {
		return nil, err
	}
	ep.Scheme = "wss"
	dialer := &websocket.Dialer{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	header := http.Header{}
	// If we have a token use it, otherwise basic auth
	if c.Token() != "" {
		header.Set("Authorization", "Bearer "+c.Token())
	} else {
		basicAuth := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
		header.Set("Authorization", "Basic "+basicAuth)
	}
	res, _, err := dialer.Dial(ep.String(), header)
	return res, err
}

// UrlProxy sets the request url proxy (for endpoint chaining)
func (c *Client) UrlProxy(up string) *Client {
	c.mux.Lock()
	defer c.mux.Unlock()
	c.urlProxy = up
	return c
}

var weAreTheProxy bool
var localProxyMux = &sync.Mutex{}

func (c *Client) makeProxy(socketPath string) (*http.Server, net.Listener, error) {
	src, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, nil, err
	}
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = src.Scheme
			req.URL.Host = src.Host
		},
		Transport:      c.Client.Transport,
		FlushInterval:  0,
		ErrorLog:       nil,
		BufferPool:     nil,
		ModifyResponse: nil,
		ErrorHandler:   nil,
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, err
	}
	return &http.Server{Handler: rp}, listener, nil
}

func (c *Client) RunProxy(socketPath string) error {
	os.Remove(socketPath)
	server, listener, err := c.makeProxy(socketPath)
	if err != nil {
		return err
	}
	return server.Serve(listener)
}

func (c *Client) MakeProxy(socketPath string) error {
	if weAreTheProxy || locallyProxied(c.neverProxy) != "" || runtime.GOOS == "windows" {
		return nil
	}
	localProxyMux.Lock()
	defer localProxyMux.Unlock()
	server, listener, err := c.makeProxy(socketPath)
	if err != nil {
		return err
	}
	trans := c.Client.Transport
	weAreTheProxy = true
	go func() {
		for {
			c.Errorf("Proxy error: %v", server.Serve(listener))
			if fi, err := os.Stat(socketPath); err != nil || fi.Mode()&os.ModeSocket == 0 {
				localProxyMux.Lock()
				defer localProxyMux.Unlock()
				c.Errorf("Proxy socket vanished!")
				os.Unsetenv(("RS_LOCAL_PROXY"))
				os.Remove(socketPath)
				c.Client.Transport = trans
				weAreTheProxy = false
				return
			}
		}
	}()
	os.Setenv("RS_LOCAL_PROXY", socketPath)
	c.Client.Transport = transport(true)
	return nil
}

func locallyProxied(neverProxy bool) string {
	if neverProxy {
		return ""
	}
	if socketPath := os.Getenv("RS_LOCAL_PROXY"); socketPath != "" {
		if fi, err := os.Stat(socketPath); err == nil && fi.Mode()&os.ModeSocket > 0 {
			return socketPath
		}
	}
	return ""
}

func transport(useproxy bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	var tr *http.Transport
	lp := locallyProxied(!useproxy)
	if lp == "" {
		tr = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   1 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DialContext: func(ctx context.Context, net, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, net, addr)
			},
		}
		http2.ConfigureTransport(tr)
	} else {
		tr = &http.Transport{
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DialContext: func(ctx context.Context, net, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", lp)
			},
		}
	}
	return tr
}

func DisconnectedClient() *Client {
	return &Client{Logger: defaultLogBuf.Log("").Fork()}
}

// TokenSessionProxy creates a new api.Client that will use the passed-in Token for authentication.
// It should be used whenever the API is not acting on behalf of a user.
// Allows for choice on session creation or not.
func TokenSessionProxy(endpoint, token string, proxy bool) (*Client, error) {
	tr := transport(proxy)
	c := DisconnectedClient()
	c.mux = &sync.Mutex{}
	c.endpoint = endpoint
	c.Client = &http.Client{Transport: tr}
	c.closer = make(chan struct{}, 0)
	c.token = &models.UserToken{Token: token}
	c.iMux = &sync.Mutex{}
	c.neverProxy = !proxy
	go func() {
		<-c.closer
		tr.CloseIdleConnections()
	}()
	return c, nil
}

// TokenSession creates a new api.Client that will use the passed-in Token for authentication.
// It should be used whenever the API is not acting on behalf of a user.
// Attempts to use/create a proxy session
func TokenSession(endpoint, token string) (*Client, error) {
	return TokenSessionProxy(endpoint, token, true)
}

// UserSessionTokenProxy allows for the token conversion turned off and turn off local proxy, along with passing in a
// context.Context to allow for faster connect timeouts.
func UserSessionTokenProxyContext(ctx context.Context, endpoint, username, password string, usetoken, useproxy bool) (*Client, error) {
	tr := transport(useproxy)
	c := DisconnectedClient()
	c.mux = &sync.Mutex{}
	c.endpoint = endpoint
	c.username = username
	c.password = password
	c.Client = &http.Client{Transport: tr}
	c.closer = make(chan struct{}, 0)
	c.iMux = &sync.Mutex{}
	c.neverProxy = !useproxy
	basicAuth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	token := &models.UserToken{}
	if err := c.Req().
		Context(ctx).
		UrlFor("users", c.username, "token").
		Headers("Authorization", "Basic "+basicAuth).
		Do(&token); err != nil {
		return nil, err
	}
	if usetoken {
		c.token = token
		go func() {
			ticker := time.NewTicker(300 * time.Second)
			for {
				select {
				case <-c.closer:
					ticker.Stop()
					tr.CloseIdleConnections()
					return
				case <-ticker.C:
					token := &models.UserToken{}
					if err := c.reauth(token); err != nil {
						if err := c.Req().
							UrlFor("users", c.username, "token").
							Headers("Authorization", "Basic "+basicAuth).
							Do(&token); err != nil {
							c.Fatalf("Error reauthing token, aborting: %v", err)
						}
					}
					c.mux.Lock()
					c.token = token
					c.mux.Unlock()
				}
			}
		}()
	}
	return c, nil
}

// UserSessionTokenProxy allows for the token conversion turned off and turn off local proxy
func UserSessionTokenProxy(endpoint, username, password string, usetoken, useproxy bool) (*Client, error) {
	return UserSessionTokenProxyContext(context.Background(), endpoint, username, password, usetoken, useproxy)
}

// UserSessionContext creates a new api.Client that can act on behalf of a
// user.  It will perform a single request using basic authentication
// to get a token that expires 600 seconds from the time the session
// is crated, and every 300 seconds it will refresh that token.
//
// UserSession does not currently attempt to cache tokens to
// persistent storage, although that may change in the future.
//
// You can use the passed-in Context to override the default connection and request/response timeouts.
func UserSessionContext(ctx context.Context, endpoint, username, password string) (*Client, error) {
	return UserSessionTokenProxyContext(ctx, endpoint, username, password, true, true)
}

// UserSession creates a new api.Client that can act on behalf of a
// user.  It will perform a single request using basic authentication
// to get a token that expires 600 seconds from the time the session
// is crated, and every 300 seconds it will refresh that token.
//
// UserSession does not currently attempt to cache tokens to
// persistent storage, although that may change in the future.
func UserSession(endpoint, username, password string) (*Client, error) {
	return UserSessionTokenProxyContext(context.Background(), endpoint, username, password, true, true)
}

// UserSessionTokenContext allows for the token conversion turned off.
// It also takes a context.Context to override the default connection timeouts.
func UserSessionTokenContext(ctx context.Context, endpoint, username, password string, usetoken bool) (*Client, error) {
	return UserSessionTokenProxyContext(ctx, endpoint, username, password, usetoken, true)
}

// UserSessionToken allows for the token conversion turned off.
func UserSessionToken(endpoint, username, password string, usetoken bool) (*Client, error) {
	return UserSessionTokenProxy(endpoint, username, password, usetoken, true)
}

func (c *Client) SetLogger(l logger.Logger) {
	c.Logger = l
}
