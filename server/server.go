package server

import (
	"esi/ast"
	"esi/tokenizer"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//https://gist.github.com/yowu/f7dc34bd4736a65ff28d
// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func delHopHeaders(header http.Header) {
	for _, h := range hopHeaders {
		header.Del(h)
	}
}

func appendHostToXForwardHeader(header http.Header, host string) {
	// If we aren't the first proxy retain prior
	// X-Forwarded-For information as a comma+space
	// separated list and fold multiple headers into one.
	if prior, ok := header["X-Forwarded-For"]; ok {
		host = strings.Join(prior, ", ") + ", " + host
	}
	header.Set("X-Forwarded-For", host)
}

func GenerateESICalls(EsiData []ast.EsiIncludeData, netClient *http.Client, ch chan string, r *http.Request) {
	calls := 0
	for i := 0; i < len(EsiData); i++ {
		for i2 := 0; i2 < len(EsiData[i].ASTData.Attributes); i2++ {
			if *EsiData[i].ASTData.Attributes[i2].Name == "src" {
				EsiData[i].URL = EsiData[i].ASTData.Attributes[i2].Value
				//================================================================
				//fmt.Println("Getting...", *EsiData[i].url)
				calls++
				go MakeRequest(&EsiData[i], *EsiData[i].URL, netClient, ch, r)
				/*
					resp, err := netClient.Get(*EsiData[i].url)
					if err != nil {
						panic(err)
					}
					defer resp.Body.Close()
					body, err := ioutil.ReadAll(resp.Body)
					bodyStr := string(body)
					EsiData[i].response = &bodyStr
				*/
				//runes := []rune(bodyStr)
				//================================================================
			} else if *EsiData[i].ASTData.Attributes[i2].Name == "ttl" {
				EsiData[i].TTL, _ = strconv.Atoi(*EsiData[i].ASTData.Attributes[i2].Name)
			}
		}
	}
	for i := 0; i < calls; i++ {
		a := <-ch
		fmt.Println(a)
	}
}

func MakeRequest(esiData *ast.EsiIncludeData, url string, netClient *http.Client, ch chan<- string, r *http.Request) {
	start := time.Now()

	//handle before ESI call handlers
	for i := 0; i < len(ESIServerConfig.BeforeESICall); i++ {
		ESIServerConfig.BeforeESICall[i].OnBeforeESICall(esiData)
	}

	//cache handling
	handled := false

	if ESIServerConfig.Cache != nil {
		cacheResp := ESIServerConfig.Cache.Get(url)
		if cacheResp != nil {
			handled = true
			esiData.Response = cacheResp
			//"not modified", used to track if we got something from cache
			esiData.ResponseCode = 304
		}

	}
	if !handled {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header = r.Header
		resp, _ := netClient.Do(req)

		body, _ := ioutil.ReadAll(resp.Body)
		bodyStr := string(body)
		esiData.Response = &bodyStr
		esiData.ResponseCode = resp.StatusCode
		if ESIServerConfig.Cache != nil {
			ESIServerConfig.Cache.Set(url, esiData.Response, esiData.TTL)
		}
	}
	secs := time.Since(start).Seconds()

	//handle after ESI call handlers
	for i := 0; i < len(ESIServerConfig.AfterESICall); i++ {
		ESIServerConfig.AfterESICall[i].OnAfterESICall(esiData)
	}
	ch <- fmt.Sprintf("%.2f elapsed with response length: %d %s", secs, len(*esiData.Response), url)

	tokens := tokenizer.ParseDocument(esiData.Response)
	fmt.Printf("%.2fs Parsing\n", time.Since(start).Seconds())
	start = time.Now()
	astree, esicalls := ast.GenerateAST(tokens)

	//attach AST to tree
	esiData.ASTData.Children = append(esiData.ASTData.Children, &astree)

	fmt.Printf("%.2fs AST Generated\n", time.Since(start).Seconds())
	start = time.Now()

	ch2 := make(chan string)
	GenerateESICalls(esicalls, netClient, ch2, r)
	close(ch2)
}

func ExecuteAST(node *ast.ASTNode, w *http.ResponseWriter, r *http.Request) {
	//if node.Token.TokenType == Root {
	//}
	if node.Token.TokenType == tokenizer.Text {
		if node.TagValue != nil && *node.TagValue != "" {
			fmt.Fprint(*w, *node.TagValue)
			//r.Write(*node.TagValue)
		}
	}

	//if node.Token.TokenType == Root {
	for i := 0; i < len(node.Children); i++ {
		//fmt.Println("Recursing...", node.Token.TokenType)
		ExecuteAST(node.Children[i], w, r)
	}
	//}
}

func getDocs(w http.ResponseWriter, r *http.Request) {

	ch := make(chan string)
	url := r.URL.Path

	var netClient = &http.Client{
		Timeout: time.Second * 10,
	}
	start := time.Now()

	delHopHeaders(r.Header)
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		appendHostToXForwardHeader(r.Header, clientIP)
	}
	req, err := http.NewRequest(r.Method, ESIServerConfig.DefaultResolver.Resolve()+url, nil)
	req.Header = r.Header
	resp, err := netClient.Do(req)
	//resp, err := netClient.Get(ESIServerConfig.DefaultResolver.Resolve() + url)
	fmt.Printf("%.2fs Doc Loaded from URL\n", time.Since(start).Seconds())
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	delHopHeaders(resp.Header)
	copyHeader(w.Header(), resp.Header)

	body, err := ioutil.ReadAll(resp.Body)
	bodyStr := string(body)

	//pageFragments := make([]string, 0, 20)
	start = time.Now()
	tokens := tokenizer.ParseDocument(&bodyStr)
	fmt.Printf("%.2fs Parsing\n", time.Since(start).Seconds())
	start = time.Now()
	astNode, esicalls := ast.GenerateAST(tokens)
	fmt.Printf("%.2fs AST Generated\n", time.Since(start).Seconds())
	start = time.Now()
	GenerateESICalls(esicalls, netClient, ch, r)
	close(ch)
	fmt.Printf("%.2fs ESI Calls\n", time.Since(start).Seconds())

	w.WriteHeader(resp.StatusCode)
	//writeAST(&ast, &w, r)
	for i := 0; i < len(astNode.Children); i++ {
		ExecuteAST(astNode.Children[i], &w, r)
	}
	//printAST(&ast, 0)
	//w.Write(body)
}

type IHealthCheck interface {
	Healthy() bool
}

type DefaultHealthCheck struct {
}

func (t DefaultHealthCheck) Healthy() bool {
	return true
}

type IResolveEntry interface {
	Resolve() string
}

type ResolveEntry struct {
	URI     string
	Healthy IHealthCheck
}

func (t ResolveEntry) Resolve() string {
	return t.URI
}

type IBeforeESICall interface {
	OnBeforeESICall(ESIData *ast.EsiIncludeData)
}
type IAfterESICall interface {
	OnAfterESICall(ESIData *ast.EsiIncludeData)
}

type ICache interface {
	TTL(key string) int
	Exists(key string) bool
	Set(key string, value *string, ttl int) bool
	Get(key string) *string
}

type ServerConfig struct {
	DefaultResolver ResolveEntry
	CallResolvers   []ResolveEntry
	AfterESICall    []IAfterESICall
	BeforeESICall   []IBeforeESICall
	Cache           ICache
}

var ESIServerConfig ServerConfig

func StartServer(address string, serverConfig ServerConfig) {
	ESIServerConfig = serverConfig
	fmt.Printf("Starting HTTP")
	router := http.NewServeMux()
	router.HandleFunc("/", getDocs)
	server := http.Server{
		Addr:    address,
		Handler: router,
	}
	err := server.ListenAndServe()
	println(err)

}