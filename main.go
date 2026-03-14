package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/mailpile/kettlingar-go/kettlingar"
)

type Response struct {
	kettlingar.ProgressUpdate
	Data string     `msgpack:"data"`
	Addr netip.Addr `msgpack:"addr"`
}

type MyService struct {
	Name string

	// These are configuration, settable via the CLI or a config file on start
	Honkey string `default:"Beep" help:"The honkey setting"`
	Tonk   int    `default:"42"   help:"The answer to everything"`

	// These match the API methods defined below, for use with MakeClient.
	Simple func(PASimpleArgs) Response
	Stream func(chan Response, PAStreamArgs)
}

func (s *MyService) GetDocs() map[string]kettlingar.MethodDesc {
	return map[string]kettlingar.MethodDesc{
		"stream": kettlingar.MethodDesc{
			Help: "Stream test",
			Docs: "This is a stream test, yessir!",
		},
		"simple": kettlingar.MethodDesc{
			Help: "Simple test",
			Docs: "This is a very simple test.\nYes it is!",
		},
	}
}

type PAStreamArgs struct {
	Count int    `default:"1"`
	Text  string `default:"Hello"`
}

func (s *MyService) PublicApiStream(out chan<- *Response, kri *kettlingar.RequestInfo, args PAStreamArgs) {
	text := args.Text
	if text == "-" {
		text = s.Honkey
	}
	for i := 0; i < args.Count; i++ {
		time.Sleep(500 * time.Millisecond)
		out <- &Response{
			ProgressUpdate: kettlingar.ProgressUpdate{
				Progress: "Made progress sending stuff!",
				IsBoth:   true,
			},
			Data: fmt.Sprintf("%s #%d iteration %s", text, s.Tonk, s.Name),
		}
	}
}

type PASimpleArgs struct {
	Text string
	Addr netip.Addr
}

func (s *MyService) ApiSimple(kri *kettlingar.RequestInfo, args PASimpleArgs) Response {
	return Response{
		Data: s.Name + ": Hello " + args.Text,
		Addr: args.Addr,
	}
}

func (r *Response) String() string {
	return fmt.Sprintf("<%s//%s>\n", r.Data, r.Addr)
}

func (r *Response) Render(mimeType string) (string, []byte) {
	switch mimeType {
	case "text/plain", "text":
		return mimeType, []byte(r.String())
	case "text/silly":
		return mimeType, []byte("<BONK!>\n")
	}
	return "text/plain", nil // Default MIME type for Response objects
}

func (svc *MyService) ServiceStartup(ks *kettlingar.KettlingarService) error {
	fmt.Printf("Service started with Honkey=%s and Tonk=%d\n", svc.Honkey, svc.Tonk)
	return nil
}

func (svc *MyService) ServiceShutdown(ks *kettlingar.KettlingarService) error {
	fmt.Printf("Service shutting down\n")
	return nil
}

func (svc *MyService) clientTest(ks *kettlingar.KettlingarService) {
	kettlingar.MakeClient(svc.Name, ks.Url, svc)

	res := svc.Simple(PASimpleArgs{Text: "kitten"})
	fmt.Println("Simple Item:", res)

	ch := make(chan Response)
	go svc.Stream(ch, PAStreamArgs{Count: 1, Text: "kitten"})
	for item := range ch {
		fmt.Println("Stream Item:", item.Data)
	}
}

func main() {
	serviceName := "my-kitten-app"
	svc := MyService{Name: serviceName}
	mux := http.NewServeMux()
	ks := kettlingar.MakeService(serviceName, "", mux, &svc)

	go (func() {
		time.Sleep(2 * time.Second) // Wait for server to start
		svc.clientTest(ks)
	})()

	ks.DefaultMain()
}
