package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	//	"github.com/mattn/go-sqlite3"

	"github.com/boltdb/bolt"
	"github.com/elazarl/goproxy"
)

func orPanic(err error) {
	if err != nil {
		panic(err)
	}
}

var dbMutex = sync.Mutex{}
var counterMutex = sync.Mutex{}
var db *bolt.DB
var counters = map[string]int{}

func add1(s string) {
	counterMutex.Lock()
	defer counterMutex.Unlock()
	counters[s] = counters[s] + 1
}

type FileStream struct {
	path string
	f    *os.File
}

func NewFileStream(path string) *FileStream {
	return &FileStream{path, nil}
}

func (fs *FileStream) Write(b []byte) (nr int, err error) {
	if fs.f == nil {
		fs.f, err = os.Create(fs.path)
		if err != nil {
			return 0, err
		}
	}
	return fs.f.Write(b)
}

func (fs *FileStream) Close() error {
	//fmt.Println("Close", fs.path)
	if fs.f == nil {
		return errors.New("FileStream was never written into")
	}
	return fs.f.Close()
}

type Meta struct {
	req      *http.Request
	resp     *http.Response
	err      error
	t        time.Time
	sess     int64
	bodyPath string
	from     string
}

func fprintf(nr *int64, err *error, w io.Writer, pat string, a ...interface{}) {
	if *err != nil {
		return
	}
	var n int
	n, *err = fmt.Fprintf(w, pat, a...)
	*nr += int64(n)
}

func write(nr *int64, err *error, w io.Writer, b []byte) {
	if *err != nil {
		return
	}
	var n int
	n, *err = w.Write(b)
	*nr += int64(n)
}

func (m *Meta) WriteTo(w io.Writer) (nr int64, err error) {
	if m.req != nil {
		fprintf(&nr, &err, w, "Type: request\r\n")
	} else if m.resp != nil {
		fprintf(&nr, &err, w, "Type: response\r\n")
	}
	fprintf(&nr, &err, w, "ReceivedAt: %v\r\n", m.t)
	fprintf(&nr, &err, w, "Session: %d\r\n", m.sess)
	fprintf(&nr, &err, w, "From: %v\r\n", m.from)
	if m.err != nil {
		// note the empty response
		fprintf(&nr, &err, w, "Error: %v\r\n\r\n\r\n\r\n", m.err)
	} else if m.req != nil {
		fprintf(&nr, &err, w, "\r\n")
		buf, err2 := httputil.DumpRequest(m.req, false)
		if err2 != nil {
			return nr, err2
		}
		write(&nr, &err, w, buf)
	} else if m.resp != nil {
		fprintf(&nr, &err, w, "\r\n")
		buf, err2 := httputil.DumpResponse(m.resp, false)
		if err2 != nil {
			return nr, err2
		}
		write(&nr, &err, w, buf)
	}
	return
}

type nopCloser struct {
	*bytes.Buffer
}

func (*nopCloser) Close() error { return nil }

// Response represents the response from an HTTP request.
//

type MyResponse struct {
	Status     string // e.g. "200 OK"
	StatusCode int    // e.g. 200
	Proto      string // e.g. "HTTP/1.0"
	ProtoMajor int    // e.g. 1
	ProtoMinor int    // e.g. 0

	// Header maps header keys to values.  If the response had multiple
	// headers with the same key, they may be concatenated, with comma
	// delimiters.  (Section 4.2 of RFC 2616 requires that multiple headers
	// be semantically equivalent to a comma-delimited sequence.) Values
	// duplicated by other fields in this struct (e.g., ContentLength) are
	// omitted from Header.
	//
	// Keys in the map are canonicalized (see CanonicalHeaderKey).
	Header http.Header

	// Body represents the response body.
	//
	// The http Client and Transport guarantee that Body is always
	// non-nil, even on responses without a body or responses with
	// a zero-length body. It is the caller's responsibility to
	// close Body.
	//
	// The Body is automatically dechunked if the server replied
	// with a "chunked" Transfer-Encoding.
	Body []byte

	// ContentLength records the length of the associated content.  The
	// value -1 indicates that the length is unknown.  Unless Request.Method
	// is "HEAD", values >= 0 indicate that the given number of bytes may
	// be read from Body.
	ContentLength int64

	// Contains transfer encodings from outer-most to inner-most. Value is
	// nil, means that "identity" encoding is used.
	TransferEncoding []string

	// Close records whether the header directed that the connection be
	// closed after reading Body.  The value is advice for clients: neither
	// ReadResponse nor Response.Write ever closes a connection.
	Close bool

	// Uncompressed reports whether the response was sent compressed but
	// was decompressed by the http package. When true, reading from
	// Body yields the uncompressed content instead of the compressed
	// content actually set from the server, ContentLength is set to -1,
	// and the "Content-Length" and "Content-Encoding" fields are deleted
	// from the responseHeader. To get the original response from
	// the server, set Transport.DisableCompression to true.
	Uncompressed bool

	// Trailer maps trailer keys to values, in the same
	// format as the header.
	Trailer http.Header

	// The Request that was sent to obtain this Response.
	// Request's Body is nil (having already been consumed).
	// This is only populated for Client requests.
	Request *http.Request

	// TLS contains information about the TLS connection on which the
	// response was received. It is nil for unencrypted responses.
	// The pointer is shared between responses and should not be
	// modified.
	TLS *tls.ConnectionState
}

type TeeReadCloser struct {
	r io.Reader
	w io.WriteCloser
	c io.Closer
}

func NewTeeReadCloser(r io.ReadCloser, w io.WriteCloser) io.ReadCloser {
	return &TeeReadCloser{io.TeeReader(r, w), w, r}
}

func (t *TeeReadCloser) Read(b []byte) (int, error) {
	return t.r.Read(b)
}

func (t *TeeReadCloser) Close() error {
	err1 := t.c.Close()
	err2 := t.w.Close()
	if err1 == nil && err2 == nil {
		return nil
	}
	if err1 != nil {
		return err2
	}
	return err1
}

type stoppableListener struct {
	net.Listener
	sync.WaitGroup
}

type stoppableConn struct {
	net.Conn
	wg *sync.WaitGroup
}

func newStoppableListener(l net.Listener) *stoppableListener {
	return &stoppableListener{l, sync.WaitGroup{}}
}

func (sl *stoppableListener) Accept() (net.Conn, error) {
	c, err := sl.Listener.Accept()
	if err != nil {
		return c, err
	}
	sl.Add(1)
	return &stoppableConn{c, &sl.WaitGroup}, nil
}

func (sc *stoppableConn) Close() error {
	//sc.wg.Done()
	return sc.Conn.Close()
}

func main() {
	verbose := flag.Bool("v", false, "should every proxy request be logged to stdout")
	addr := flag.String("l", ":8080", "on which address should the proxy listen")
	flag.Parse()
	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile("baidu.*:443$"))).
		HandleConnect(goproxy.AlwaysReject)
	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile("^.*$"))).
		HandleConnect(goproxy.AlwaysMitm)

	proxy.Verbose = *verbose
	if err := os.MkdirAll("db", 0755); err != nil {
		log.Fatal("Can't create dir", err)
	}
	var err error
	//tr := transport.Transport{Proxy: transport.ProxyFromEnvironment}
	/*
		proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			ctx.RoundTripper = goproxy.RoundTripperFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (resp *http.Response, err error) {
				ctx.UserData, resp, err = tr.DetailedRoundTrip(req)
				return
			})
			return req, nil
			//logger.LogReq(req, ctx)
			url := req.URL.String()
			log.Println("Starting request handler on", *addr)
			//dbMutex.Lock()
			//defer dbMutex.Unlock()

			var packedResp []byte

			db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte("MyBucket"))
				if b == nil {
					log.Println("Failed to get bucket")
					return fmt.Errorf("No bucket")
				}
				//b = tx.Bucket([]byte("MyBucket"))

				log.Println("URL ", url)

				packedResp = b.Get([]byte(url))
				return nil
			})
			if packedResp != nil {

				var resp = MyResponse{}
				//anotherbuffer := bytes.NewBuffer(packedResp)

				err = json.Unmarshal(packedResp, &resp)
				if err != nil {
					log.Printf("Error decoding response from cache: %v\n", err)
				}
				//log.Printf("Returning %v\n", resp)
				body := bytes.NewBuffer(resp.Body)
				bodyData, _ := ioutil.ReadAll(body)
				bytes.ReplaceAll(bodyData, []byte("https"), []byte("http"))
				body = bytes.NewBuffer(bodyData)
				var myResp = http.Response{
					resp.Status,
					resp.StatusCode,
					resp.Proto,
					resp.ProtoMajor,
					resp.ProtoMinor,
					resp.Header,
					&nopCloser{body}, //body
					resp.ContentLength,
					resp.TransferEncoding,
					resp.Close,
					resp.Uncompressed,
					resp.Trailer,
					req,
					resp.TLS}
				add1("hit")
				log.Println("Using cached copy")
				return req, &myResp

			} else {
				log.Println("Retrieve from cache failed or file not stored")
			}
			log.Println("passing request")

			//log.Printf("Wrote (%s), %v\n", url, data)
			add1("miss")
			log.Println(counters)
			return req, nil
		})
	*/
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		fmt.Printf("Response: %+v\n", resp)
		data, _ := ioutil.ReadAll(resp.Body)
		//fmt.Printf("Body: %v\n", string(data))

		fmt.Printf("[%d] %s %s\n", resp.StatusCode, ctx.Req.Method, ctx.Req.URL)
		u := ctx.Req.URL.String()
		upath := ctx.Req.URL.Path

		if upath == "" {
			upath = upath + "/index.html"
		}
		if upath == "." {
			upath = upath + "index.html"
		}
		if strings.HasSuffix(upath, "/") {
			upath = upath + "index.html"
		}
		path := filepath.Clean(upath)
		uobj, _ := url.Parse(u)
		path = "rip/" + uobj.Hostname() + "/" + path
		fmt.Printf("%v\n", path)
		dir := filepath.Dir(path)
		os.MkdirAll(dir, 0600)
		ioutil.WriteFile(path, data, 0600)
		r := bytes.NewReader(data)
		resp.Body = ioutil.NopCloser(r)
		return resp
	})

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal("listen:", err)
	}
	sl := newStoppableListener(l)
	ch := make(chan os.Signal)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		log.Println("Got SIGINT exiting")
		os.Exit(1)
		sl.Add(1)
		sl.Close()

		sl.Done()
	}()

	db, err = bolt.Open("webcache.bolt", 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	//defer db.Close()
	log.Println("Starting Proxy on", *addr)
	http.Serve(sl, proxy)
	sl.Wait()
	log.Println("All connections closed - exit")
}
