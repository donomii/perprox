package main

import (
	"regexp"
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path"
	"sync"
	"time"

	//	"github.com/mattn/go-sqlite3"

	"github.com/boltdb/bolt"
	"github.com/elazarl/goproxy"
	"github.com/elazarl/goproxy/transport"
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

type HttpLogger struct {
	path  string
	c     chan *Meta
	errch chan error
}

func NewLogger(basepath string) (*HttpLogger, error) {
	f, err := os.Create(path.Join(basepath, "log"))
	if err != nil {
		return nil, err
	}
	logger := &HttpLogger{basepath, make(chan *Meta), make(chan error)}
	go func() {
		for m := range logger.c {
			if _, err := m.WriteTo(f); err != nil {
				log.Println("Can't write meta", err)
			}
		}
		logger.errch <- f.Close()
	}()
	return logger, nil
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

func (logger *HttpLogger) LogResp(resp *http.Response, ctx *goproxy.ProxyCtx) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered in f", r)
		}
	}()

	//log.Printf("Response: %v\n", resp)

	//re := regexp.MustCompile("[[:cntrl:]]|[\"#$%&'()*+,\\-/:;<=>?@[\\\\\\]^_`{|}~]")
	//fname := re.ReplaceAllString(url, "_")
	//log.Println("Saving to file: ", fname)
	//body := path.Join(logger.path, fmt.Sprintf("resp_%s", fname))
	from := ""
	if ctx.UserData != nil {
		from = ctx.UserData.(*transport.RoundTripDetails).TCPAddr.String()
	}
	dbMutex.Lock()
	defer dbMutex.Unlock()
	var buff1 bytes.Buffer
	var buff2 bytes.Buffer
	if resp == nil {
		resp = emptyResp
	} else {
		buff2.ReadFrom(NewTeeReadCloser(resp.Body, &nopCloser{&buff1}))
		resp.Body = &nopCloser{&buff2}
		data := buff1.Bytes()
		if len(data) > 0 {

			req := ctx.Req
			url := req.URL.String()

			var buff3 bytes.Buffer // Stand-in for a network connection
			enc := gob.NewEncoder(&buff3)
			var myResp = MyResponse{
				resp.Status,
				resp.StatusCode,
				resp.Proto,
				resp.ProtoMajor,
				resp.ProtoMinor,
				resp.Header,
				data, //body
				resp.ContentLength,
				resp.TransferEncoding,
				resp.Close,
				resp.Uncompressed,
				resp.Trailer,
				req,
				resp.TLS}
			err1 := enc.Encode(myResp)
			if err1 != nil {
				log.Printf("Failed to encode response! %v\n", err1)
			}

			db.Update(func(tx *bolt.Tx) error {
				b, err := tx.CreateBucketIfNotExists([]byte("MyBucket"))
				//b = tx.Bucket([]byte("MyBucket"))
				b.Delete([]byte(url))
				data2 := buff3.Bytes()
				err = b.Put([]byte(url), data2)
				//log.Printf("Wrote (%s), %v\n", url, data)
				return err
			})
		}
	}
	//args := sqlite3.NamedArgs{"$a": url, "$b": "dunno", "$c": buff1}
	//db.Exec("INSERT INTO pages VALUES($a, $b, $c)", args)
	logger.LogMeta(&Meta{
		resp: resp,
		err:  ctx.Error,
		t:    time.Now(),
		sess: ctx.Session,
		from: from})

}

var emptyResp = &http.Response{}
var emptyReq = &http.Request{}

func (logger *HttpLogger) LogReq(req *http.Request, ctx *goproxy.ProxyCtx) {
	body := path.Join(logger.path, fmt.Sprintf("%d_req", ctx.Session))
	if req == nil {
		req = emptyReq
	} else {
		req.Body = NewTeeReadCloser(req.Body, NewFileStream(body))
	}
	logger.LogMeta(&Meta{
		req:  req,
		err:  ctx.Error,
		t:    time.Now(),
		sess: ctx.Session,
		from: req.RemoteAddr})
}

func (logger *HttpLogger) LogMeta(m *Meta) {
	logger.c <- m
}

func (logger *HttpLogger) Close() error {
	close(logger.c)
	return <-logger.errch
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


	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile("^.*:443$"))).
		HijackConnect(func(req *http.Request, client net.Conn, ctx *goproxy.ProxyCtx) {
		defer func() {
			if e := recover(); e != nil {
				ctx.Logf("error connecting to remote: %v", e)
				client.Write([]byte("HTTP/1.1 500 Cannot reach destination\r\n\r\n"))
			}
			client.Close()
		}()
		log.Println("Intercepting https")
		clientBuf := bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client))
		remote, err := net.Dial("tcp", req.URL.Host)
		orPanic(err)
		remoteBuf := bufio.NewReadWriter(bufio.NewReader(remote), bufio.NewWriter(remote))
		for {
			req, err := http.ReadRequest(clientBuf.Reader)
			orPanic(err)
			orPanic(req.Write(remoteBuf))
			orPanic(remoteBuf.Flush())
			resp, err := http.ReadResponse(remoteBuf.Reader, req)
			orPanic(err)
			orPanic(resp.Write(clientBuf.Writer))
			orPanic(clientBuf.Flush())
		}
	})





	proxy.Verbose = *verbose
	if err := os.MkdirAll("db", 0755); err != nil {
		log.Fatal("Can't create dir", err)
	}
	logger, err := NewLogger("db")
	if err != nil {
		log.Fatal("can't open log file", err)
	}
	tr := transport.Transport{Proxy: transport.ProxyFromEnvironment}
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (resp *http.Response, err error) {
			ctx.UserData, resp, err = tr.DetailedRoundTrip(req)
			return
		})
		logger.LogReq(req, ctx)
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
			anotherbuffer := bytes.NewBuffer(packedResp)

			dec := gob.NewDecoder(anotherbuffer)
			err = dec.Decode(&resp)
			if err != nil {
				log.Printf("Error decoding response from cache: %v\n", err)
			}
			//log.Printf("Returning %v\n", resp)
			body := bytes.NewBuffer(resp.Body)

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
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		logger.LogResp(resp, ctx)
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
		logger.Close()
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
