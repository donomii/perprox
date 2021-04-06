package main

import (
	"bytes"
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

	//	"github.com/mattn/go-sqlite3"

	"github.com/donomii/goof"
	"github.com/elazarl/goproxy"
)

var stripchatmodels map[string]string = map[string]string{}

type AppendRequest struct {
	Path string
	Data []byte
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

var appendHandleCache = map[string]*os.File{}

func appendWorker() {
	for req := range appendChan {
		path := req.Path
		path = strings.Replace(path, ":", "", -1)
		path = strings.Replace(path, "\\", "/", -1)
		path = strings.Replace(path, "//", "/", -1)

		log.Printf("Appending to path %v\n", path)

		f, ok := appendHandleCache[path]
		var err error
		if !ok {
			dir := filepath.Dir(path)
			os.MkdirAll(dir, 0600)
			f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)

			if err != nil {
				log.Println("Error opening file: ", err)
			}
			appendHandleCache[path] = f
		}
		go func(data []byte, path string, f *os.File) {
			_, err = f.Write(data)
			if err != nil {
				log.Println("Failed to write to ", path, " because ", err)
			}
		}(req.Data, path, f)

		//f.Close()
	}
}

func responseHandlerFunc(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	fmt.Printf("Response: %+v\n", resp)
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return resp
	}
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

	log.Println("Examining path ", path)

	uobj, err := url.Parse(u)
	if err != nil {
		r := bytes.NewReader(data)
		resp.Body = ioutil.NopCloser(r)
		return resp
	}

	//https: //stripchat.com/api/front/models/36737951/viewers/-5783  //referrer
	if strings.Contains(path, "api/front/models") {
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
		log.Println("Found stream in ", path)
		re := regexp.MustCompile(`amlst:(.+)-`)
		streamName := string(re.Find([]byte(path)))
		streamName = strings.TrimPrefix(streamName, "amlst:")
		streamName = strings.TrimSuffix(streamName, "-sd-")
		if err == nil {

			path = "streams/" + streamName + ".ts"

			appendChan <- AppendRequest{path, data}
		} else {
			log.Println("Error naming stream:", err)
		}
	} else if strings.Contains(path, "/hls/") && strings.HasSuffix(path, ".ts") {
		log.Println("Found stream in ", path)
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
			appendChan <- AppendRequest{path, data}
		} else {
			log.Println("Error naming stream:", err)
		}
	} else {
		path = "rip/" + uobj.Hostname() + "/" + path
		path = strings.Replace(path, ":", "", -1)
		path = strings.Replace(path, "\\", "/", -1)
		path = strings.Replace(path, "//", "/", -1)

		dir := filepath.Dir(path)
		filename := filepath.Base(path)
		os.MkdirAll(dir, 0600)
		if goof.Exists(path) && filename == "stream" {

			appendChan <- AppendRequest{path, data}
		} else {
			go func() {
				fmt.Printf("Saving to path %v\n", path)
				//ioutil.WriteFile(path, data, 0600)
			}()
		}
	}
	r := bytes.NewReader(data)
	resp.Body = ioutil.NopCloser(r)
	return resp
}

func main() {
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
