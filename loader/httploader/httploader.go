package httploader

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"

	"github.com/cshum/imagor"
)

// HTTPLoader HTTP Loader implements imagor.Loader interface
type HTTPLoader struct {
	// The Transport used to request images, default http.DefaultTransport.
	Transport http.RoundTripper

	// ForwardHeaders copy request headers to image request headers
	ForwardHeaders []string

	// OverrideHeaders override image request headers
	OverrideHeaders map[string]string

	// AllowedSources list of host names allowed to load from,
	// supports glob patterns such as *.google.com
	AllowedSources []string

	// Accept set request Accept and validate response Content-Type header
	Accept string

	// MaxAllowedSize maximum bytes allowed for image
	MaxAllowedSize int

	// DefaultScheme default image URL scheme
	DefaultScheme string

	// UserAgent default user agent for image request.
	// Can be overridden by ForwardHeaders and OverrideHeaders
	UserAgent string

	// BlockLoopbackNetworks rejects HTTP connections to loopback network IP addresses.
	BlockLoopbackNetworks bool

	// BlockPrivateNetworks rejects HTTP connections to private network IP addresses.
	BlockPrivateNetworks bool

	// BlockLinkLocalNetworks rejects HTTP connections to link local IP addresses.
	BlockLinkLocalNetworks bool

	// BlockNetworks rejects HTTP connections to a configurable list of networks.
	BlockNetworks []*net.IPNet

	accepts []string
}

// New creates HTTPLoader
func New(options ...Option) *HTTPLoader {
	h := &HTTPLoader{
		OverrideHeaders: map[string]string{},
		DefaultScheme:   "https",
		Accept:          "*/*",
		UserAgent:       fmt.Sprintf("imagor/%s", imagor.Version),
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Control: h.DialControl}
	transport.DialContext = dialer.DialContext
	h.Transport = transport

	for _, option := range options {
		option(h)
	}
	if s := strings.ToLower(h.DefaultScheme); s == "nil" {
		h.DefaultScheme = ""
	}
	if h.Accept != "" {
		for _, seg := range strings.Split(h.Accept, ",") {
			if typ := parseContentType(seg); typ != "" {
				h.accepts = append(h.accepts, typ)
			}
		}
	}
	return h
}

// Get implements imagor.Loader interface
func (h *HTTPLoader) Get(r *http.Request, image string) (*imagor.Blob, error) {
	if image == "" {
		return nil, imagor.ErrInvalid
	}
	u, err := url.Parse(image)
	if err != nil {
		return nil, imagor.ErrInvalid
	}
	if u.Host == "" || u.Scheme == "" {
		if h.DefaultScheme != "" {
			image = h.DefaultScheme + "://" + image
			if u, err = url.Parse(image); err != nil {
				return nil, imagor.ErrInvalid
			}
		} else {
			return nil, imagor.ErrInvalid
		}
	}
	if !isURLAllowed(u, h.AllowedSources) {
		return nil, imagor.ErrInvalid
	}
	client := &http.Client{
		Transport:     h.Transport,
		CheckRedirect: h.checkRedirect,
	}
	if h.MaxAllowedSize > 0 {
		req, err := h.newRequest(r, http.MethodHead, image)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 200 && resp.StatusCode > 206 {
			return nil, imagor.NewErrorFromStatusCode(resp.StatusCode)
		}
		contentLength, _ := strconv.Atoi(resp.Header.Get("Content-Length"))
		if contentLength > h.MaxAllowedSize {
			return nil, imagor.ErrMaxSizeExceeded
		}
	}
	req, err := h.newRequest(r, http.MethodGet, image)
	if err != nil {
		return nil, err
	}
	return imagor.NewBlob(func() (io.ReadCloser, int64, error) {
		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(err, ErrUnauthorizedRequest) {
				err = imagor.NewError(
					fmt.Sprintf("%s: %s", err.Error(), image),
					http.StatusForbidden)
			} else if idx := strings.Index(err.Error(), "dial tcp: "); idx > -1 {
				err = imagor.NewError(
					fmt.Sprintf("%s: %s", err.Error()[idx:], image),
					http.StatusNotFound)
			}
			return nil, 0, err
		}
		body := resp.Body
		size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
		if resp.Header.Get("Content-Encoding") == "gzip" {
			gzipBody, err := gzip.NewReader(resp.Body)
			if err != nil {
				return nil, 0, err
			}
			body = gzipBody
			size = 0 // size unknown after decompress
		}
		if resp.StatusCode >= 400 {
			return body, size, imagor.NewErrorFromStatusCode(resp.StatusCode)
		}
		if !validateContentType(resp.Header.Get("Content-Type"), h.accepts) {
			return body, size, imagor.ErrUnsupportedFormat
		}
		return body, size, nil
	}), nil
}

func (h *HTTPLoader) newRequest(r *http.Request, method, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", h.UserAgent)
	if h.Accept != "" {
		req.Header.Set("Accept", h.Accept)
	}
	for _, header := range h.ForwardHeaders {
		if header == "*" {
			req.Header = r.Header.Clone()
			req.Header.Del("Accept-Encoding") // fix compressions
			break
		}
		if _, ok := r.Header[header]; ok {
			req.Header.Set(header, r.Header.Get(header))
		}
	}
	for key, value := range h.OverrideHeaders {
		req.Header.Set(key, value)
	}
	return req, nil
}

func (h *HTTPLoader) checkRedirect(r *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !isURLAllowed(r.URL, h.AllowedSources) {
		return imagor.ErrInvalid
	}
	return nil
}

// ErrUnauthorizedRequest unauthorized request error
var ErrUnauthorizedRequest = errors.New("unauthorized request")

// DialControl implements a net.Dialer.Control function which is automatically used with the default http.Transport.
// If the transport is replaced using the WithTransport option it is up to that
// transport if the control function is used or not.
func (h *HTTPLoader) DialControl(network string, address string, conn syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr := net.ParseIP(host)
	if h.BlockLoopbackNetworks && addr.IsLoopback() {
		return ErrUnauthorizedRequest
	}
	if h.BlockLinkLocalNetworks && (addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast()) {
		return ErrUnauthorizedRequest
	}
	if h.BlockPrivateNetworks && addr.IsPrivate() {
		return ErrUnauthorizedRequest
	}
	for _, network := range h.BlockNetworks {
		if network.Contains(addr) {
			return ErrUnauthorizedRequest
		}
	}
	return nil
}
