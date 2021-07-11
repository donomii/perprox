package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	//	"github.com/mattn/go-sqlite3"

	"github.com/donomii/goof"
	"github.com/elazarl/goproxy"
)

var stripchatmodels map[string]string = map[string]string{}

type AppendRequest struct {
	Path     string
	SavePipe io.Reader
}

var appendChan chan AppendRequest = make(chan AppendRequest, 1000)

func orPanic(err error) {
	if err != nil {
		panic(err)
	}
}

var counterMutex = sync.Mutex{}
var counters = map[string]int{}

func add1(s string) {
	counterMutex.Lock()
	defer counterMutex.Unlock()
	counters[s] = counters[s] + 1
}

func fprintf(nr *int64, err *error, w io.Writer, pat string, a ...interface{}) {
	if *err != nil {
		return
	}
	var n int
	n, *err = fmt.Fprintf(w, pat, a...)
	*nr += int64(n)
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

var appendHandleCache = map[string]*bufio.Writer{}
var appendChanCache = map[string]*chan AppendRequest{}

func fileWriteWorker(fileChan chan AppendRequest, f *bufio.Writer) {
	for req := range fileChan {
		go io.Copy(f, req.SavePipe)
	}
}

func normalisePath(path string) string {
	path = strings.Replace(path, ":", "", -1)
	path = strings.Replace(path, "\\", "/", -1)
	path = strings.Replace(path, "//", "/", -1)

	path = strings.Replace(path, "=", "", -1)
	path = strings.Replace(path, ".com", "", -1)
	return path
}
func appendWorker() {
	for req := range appendChan {
		path := normalisePath(req.Path)

		log.Printf("Appending to path %v\n", path)
		var ch chan AppendRequest
		chp, ok := appendChanCache[path]
		if ok {
			ch := *chp
			ch <- req
		} else {
			dir := filepath.Dir(path)
			os.MkdirAll(dir, 0600)
			fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)

			if err != nil {
				log.Println("Error opening file: ", err)
			} else {

				f := bufio.NewWriterSize(fh, 10000)
				appendHandleCache[path] = f
				ch = make(chan AppendRequest, 1)
				appendChanCache[path] = &ch
				go fileWriteWorker(ch, f)
				ch <- req
				log.Println("Created new writepipe for", path)
			}
		}

		//f.Close()
	}
}

func SendToAppend(req AppendRequest) {
	path := normalisePath(req.Path)

	chp, ok := appendChanCache[path]
	if ok {
		//log.Println("Found cached write pipe for", path)
		ch := *chp
		ch <- req
	} else {
		log.Println("Creating new writepipe for", path)
		appendChan <- req
	}
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

func responseHandlerFunc(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	//log.Printf("Response: %+v\n", resp)
	bufIn := bufio.NewReader(resp.Body)
	savePipe, savePipew := io.Pipe()
	respData := NewTeeReadCloser(ioutil.NopCloser(bufIn), savePipew)
	go func() {
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
		path = strings.Replace(path, "\\", "/", -1)

		//log.Println("Examining path ", path)

		uobj, err := url.Parse(u)
		if err != nil {
			return
		}

		//https: //stripchat.com/api/front/models/36737951/viewers/-5783  //referrer
		if strings.Contains(path, "api/front/models") {
			log.Println("Stripchat url", path)
			referrer := resp.Request.Referer()
			referrer = strings.TrimPrefix(referrer, `https://stripchat.com/`)
			bits := strings.Split(referrer, "?")
			modelName := bits[0]

			re := regexp.MustCompile(`front/models/(.+)/apps`)

			streamName := string(re.Find([]byte(path)))
			streamName = strings.TrimPrefix(streamName, "front/models/")
			streamName = strings.TrimSuffix(streamName, "/apps")
			if streamName != "" {
				stripchatmodels[streamName] = modelName
				log.Println("mapped stream id ", streamName, " to model ", modelName)
			}
		} else if strings.Contains(path, "live-hls") && strings.HasSuffix(path, ".ts") {
			log.Println("Found chaturbate in ", path)
			re := regexp.MustCompile(`amlst:(.+)-`)
			streamName := string(re.Find([]byte(path)))
			streamName = strings.TrimPrefix(streamName, "amlst:")
			streamName = strings.TrimSuffix(streamName, "-sd-")
			if err == nil {

				path = "streams/" + streamName + ".ts"

				SendToAppend(AppendRequest{path, savePipe})
			} else {
				log.Println("Error naming stream:", err)
			}
		} else if strings.Contains(path, "/hls/") && strings.HasSuffix(path, ".ts") {
			log.Println("Found stripchat  in ", path)
			re := regexp.MustCompile(`hls/(.+).ts`)
			streamName := string(re.Find([]byte(path)))
			streamName = strings.TrimPrefix(streamName, "hls/")
			streamName = strings.TrimSuffix(streamName, ".ts")
			bits := strings.Split(streamName, "/")
			streamName = bits[0]
			modelName, ok := stripchatmodels[streamName]
			if !ok {
				modelName = streamName
			}
			if err == nil {
				path = "streams/" + modelName + ".ts"
				SendToAppend(AppendRequest{path, savePipe})
			} else {
				log.Println("Error naming stream:", err)
			}
		} else if strings.Contains(path, "/videoplayback") {
			log.Println("Found youtube  in ", path)
			mimeType := ctx.Req.URL.Query().Get("mime")
			log.Println("Mime type:", mimeType)
			suffix := ".ts"
			if strings.HasPrefix(mimeType, "audio/") {
				suffix = "_audio." + strings.TrimPrefix(mimeType, "audio/")
			}
			if strings.HasPrefix(mimeType, "video/") {
				suffix = "_video." + strings.TrimPrefix(mimeType, "video/")
			}
			path = ctx.Req.URL.Query().Get("ei")
			streamName := path // path[:20]
			log.Println("Streamname  ", streamName)

			path = "streams/" + streamName + suffix
			log.Println("Saving youtube to ", path)
			SendToAppend(AppendRequest{path, savePipe})

		} else {
			path = "rip/" + uobj.Hostname() + "/" + path
			path = strings.Replace(path, ":", "", -1)
			path = strings.Replace(path, "\\", "/", -1)
			path = strings.Replace(path, "//", "/", -1)

			dir := filepath.Dir(path)
			filename := filepath.Base(path)
			os.MkdirAll(dir, 0600)
			if goof.Exists(path) && filename == "stream" {
				//SendToAppend(AppendRequest{path, savePipe})
				go io.CopyBuffer(ioutil.Discard, savePipe, nil)
			} else {
				if goof.Exists(path) && filename == "videoplayback" {
					SendToAppend(AppendRequest{"streams/videoplayback.ts", savePipe})
					//go io.CopyBuffer(ioutil.Discard, savePipe, nil)
				} else {
					if strings.Contains(path, ".m3u8") {
						go io.CopyBuffer(ioutil.Discard, savePipe, nil)
					} else {
						go func() {
							path := normalisePath(path)
							log.Printf("Saving whole file to path %v\n", path)
							dir := filepath.Dir(path)
							//log.Printf("makedir %v\n", dir)
							os.MkdirAll(dir, 0600)
							f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
							if err != nil {
								log.Println(err, path)
								go io.CopyBuffer(ioutil.Discard, savePipe, nil)
								return
							}
							defer f.Close()
							io.CopyBuffer(f, savePipe, nil)
							f.Close()
							savePipe.Close()
						}()
					}
				}
			}
		}
	}()
	resp.Body = ioutil.NopCloser(respData)
	return resp
}

func cleanup() {
	for _, f := range appendHandleCache {
		f.Flush()
	}
}

func main() {

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		cleanup()
		os.Exit(1)
	}()

	go appendWorker()
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

	proxy.OnResponse().DoFunc(responseHandlerFunc)

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

	//defer db.Close()
	log.Println("Starting Proxy on", *addr)
	http.Serve(sl, proxy)
	sl.Wait()
	log.Println("All connections closed - exit")
}
