package kettlingar

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

var KettlingarVersion string = "v0.0.1"

type MethodDesc struct {
	Name        string            `msgpack:"name" json:"name"`
	Help        string            `msgpack:"help" json:"help"`
	Docs        string            `msgpack:"docs" json:"docs"`
	Args        map[string]string `msgpack:"args" json:"args"`
	ArgDefaults map[string]string `msgpack:"arg_defaults" json:"arg_default"`
	DefaultMime string            `msgpack:"default_mimetype" json:"default_mimetype"`
	ReturnType  reflect.Type      `msgpack:"-" json:"-"`
	Returns     string            `msgpack:"returns" json:"returns"`
	IsGenerator bool              `msgpack:"is_generator" json:"is_generator"`
	IsPublic    bool              `msgpack:"is_public" json:"is_public"`
}

type DocumentedAPI interface {
	GetDocs() map[string]MethodDesc
}

type KettlingarService struct {
	Name        string
	Secret      string
	Url         string
	StateFn     string
	Version     string
	Mux         *http.ServeMux
	toJson      *MsgpackJsonConverter
	services    []interface{}
	registry    []MethodDesc
	metrics     *Metrics
	metricsPriv *Metrics
	Logger      *slog.Logger
}

type ProgressUpdate struct {
	Timestamp time.Time `msgpack:"ts" json:"ts"`
	Progress  string    `msgpack:"progress" json:"progress"`
	IsError   bool      `msgpack:"error_code" json:"error_code"`
	IsBoth    bool      `msgpack:"progress_both" json:"progress_both"`
}

type DataRenderer interface {
	Render(string) (string, []byte)
}

type RequestInfo struct {
	Timestamp time.Time
	HttpCode  int
	Service   *KettlingarService
	Method    *MethodDesc
	Request   *http.Request
	Writer    http.ResponseWriter
	IsAuthed  bool
}

func MakeService(name, secret string, mux *http.ServeMux, service interface{}) *KettlingarService {
	if secret == "" {
		if s, err := generateSecret(); err == nil {
			secret = s
		} else {
			return nil
		}
	}
	ks := &KettlingarService{
		Name:        name,
		Secret:      secret,
		Version:     KettlingarVersion,
		Mux:         mux,
		services:    make([]interface{}, 0),
		registry:    make([]MethodDesc, 0),
		toJson:      NewJsonConverter(),
		metrics:     NewMetrics(),
		metricsPriv: NewMetrics(),
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := ks.RegisterService(&DefaultMethods{}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register default methods! %v\n", err)
		return nil
	}
	if err := ks.RegisterService(&MetricsMethods{}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register metrics methods! %v\n", err)
		return nil
	}
	if err := ks.RegisterService(service); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register service methods! %v\n", err)
		return nil
	}

	ks.RegisterMsgpackExtensions()

	apiHash := sha256.Sum256(ks.ForgivingJSON(ks.registry))
	ks.Version = fmt.Sprintf("%s.%x", KettlingarVersion, apiHash[:4])

	ks.metrics.Help["started"] = "Service information"
	ks.metrics.Help["start_ts"] = "Service start time, Unix timestamp"
	ks.metrics.Help["rpc_calls_total"] = "Total processed RPC requests"
	ks.metrics.Help["rpc_calls_time_us"] = "RPC runtime in microseconds"
	ks.MetricsGuage("start_ts", uint64(time.Now().Unix()), PrivateMetric, nil)
	ks.MetricsInfo("started", name, PublicMetric, MetricLabels{
		"version": ks.Version,
	})

	return ks
}

func (ks *KettlingarService) RegisterMsgpackExtensions() {
	//msgpack.RegisterExtEncoder(16, reflect.TypeOf((*WrappedAddr)(nil)).Elem(), func(e *msgpack.Encoder, v reflect.Value) ([]byte, error) {
	//	addr := v.Interface().(WrappedAddr)
	//	out := []byte(addr.Addr.String())
	//	return out, nil
	//})
	//msgpack.RegisterExtDecoder(16, reflect.TypeOf((*WrappedAddr)(nil)).Elem(), func(d *msgpack.Decoder, v reflect.Value, extLen int) error {
	//	data := make([]byte, extLen)
	//	_, err := d.Buffered().Read(data)
	//	if err != nil {
	//		return fmt.Errorf("failed to read msgpack extension data: %w", err)
	//	}
	//	var addr netip.Addr
	//	if err := addr.UnmarshalBinary(data); err != nil {
	//		return fmt.Errorf("failed to unmarshal netip.Addr: %w", err)
	//	}
	//      wAddr := WrappedAddr{Addr: addr}

	//	v.Set(reflect.ValueOf(wAddr))
	//	return nil
	//})
	//ks.toJson.ExtensionNames[16] = "netip.Addr"
}

func (ks *KettlingarService) GetApiManifest() []MethodDesc {
	return ks.registry
}

func (ks *KettlingarService) RegisterService(service interface{}) error {
	ks.services = append(ks.services, service)

	svcType := reflect.TypeOf(service)
	svcVal := reflect.ValueOf(service)

	var docs map[string]MethodDesc
	if d, ok := service.(DocumentedAPI); ok {
		docs = d.GetDocs()
	}

	for i := 0; i < svcType.NumMethod(); i++ {
		method := svcType.Method(i)
		var mName string
		var isPublic bool

		if strings.HasPrefix(method.Name, "PublicApi") {
			isPublic = true
			mName = strings.ToLower(strings.TrimPrefix(method.Name, "PublicApi"))
		} else if strings.HasPrefix(method.Name, "Api") {
			isPublic = false
			mName = strings.ToLower(strings.TrimPrefix(method.Name, "Api"))
		} else {
			continue
		}

		numIn := method.Type.NumIn() - 1
		if numIn < 1 || 3 < numIn {
			return errors.New(fmt.Sprintf(
				"%s() has wrong number of arguments (%d)",
				method.Name, numIn))
		}
		firstArg := method.Type.In(1)

		desc := MethodDesc{}
		if d, ok := docs[mName]; ok {
			desc = d
		}
		desc.Name = mName
		desc.Args = make(map[string]string)
		desc.ArgDefaults = make(map[string]string)
		desc.IsPublic = isPublic
		desc.Returns = "void"
		desc.DefaultMime = "application/json"

		chanArg := firstArg
		infoArg := firstArg
		argsIdx := 2
		if numIn >= 2 {
			if chanArg.Kind() == reflect.Chan {
				if chanArg.ChanDir() == reflect.SendDir {
					desc.IsGenerator = true
					infoArg = method.Type.In(2)
					argsIdx = 3
				} else {
					return errors.New(fmt.Sprintf(
						"%s() takes wrong kind of chan as first argument",
						method.Name))
				}
			}
		}
		if infoArg != reflect.TypeOf(&RequestInfo{}) {
			return errors.New(fmt.Sprintf(
				"%s() should take *RequestInfo as first non-chan argument",
				method.Name))
		}
		if numIn > argsIdx {
			return errors.New(fmt.Sprintf("%s() takes too many arguments: %d > %d",
				method.Name, numIn, argsIdx))
		}

		if numIn == argsIdx {
			argsArg := method.Type.In(argsIdx)
			if argsArg.Kind() != reflect.Struct {
				return errors.New(fmt.Sprintf("%s() final argument should be a struct",
					method.Name))
			}

			for j := 0; j < argsArg.NumField(); j++ {
				field := argsArg.Field(j)
				name := field.Tag.Get("msgpack")
				if name == "" {
					name = field.Name
				}

				if field.Type == reflect.TypeOf(netip.Addr{}) {
					desc.Args[name] = "netip.Addr"
				} else {
					desc.Args[name] = field.Type.Name()
				}

				defaultValue := field.Tag.Get("default")
				if defaultValue != "" {
					desc.ArgDefaults[name] = defaultValue
				}
			}
		}

		if desc.IsGenerator {
			desc.ReturnType = getIndirectType(chanArg.Elem())
			desc.Returns = desc.ReturnType.Name()
		} else {
			if method.Type.NumOut() > 0 {
				desc.ReturnType = getIndirectType(method.Type.Out(0))
				desc.Returns = desc.ReturnType.Name()
			}
		}

		// If we support the DataRenderer interface, query for the
		// default MIME type.
		valPtr := reflect.New(desc.ReturnType)
		valIf := valPtr.Interface()
		if v, ok := valIf.(DataRenderer); ok {
			mt, _ := v.Render("")
			desc.DefaultMime = mt
		}

		ks.registry = append(ks.registry, desc)

		idx := i
		handler := func(w http.ResponseWriter, r *http.Request) {
			authed := ks.authenticate(r, mName)
			if !isPublic && !authed {
				http.Error(w, "404 Not Found", http.StatusNotFound)
				return
			}
			ri := &RequestInfo{
				Timestamp: time.Now(),
				HttpCode:  0,
				Service:   ks,
				Method:    &desc,
				Request:   r,
				Writer:    w,
				IsAuthed:  authed,
			}
			ks.handleRPC(ri, svcVal.Method(idx), method.Type)
		}

		ks.Mux.HandleFunc("/"+ks.Secret+"/"+mName, handler)
		ks.Mux.HandleFunc("/"+mName, handler)
	}

	return nil
}

func (ks *KettlingarService) authenticate(r *http.Request, methodName string) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") && strings.TrimPrefix(auth, "Bearer ") == ks.Secret {
		return true
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == ks.Secret && parts[1] == methodName {
		return true
	}
	return false
}

func (ks *KettlingarService) handleRPC(ri *RequestInfo, methodVal reflect.Value, methodType reflect.Type) {
	defer func() {
		if r := recover(); (r != nil) || (ri.HttpCode == 0) {
			ri.HttpCode = http.StatusInternalServerError
			http.Error(ri.Writer, "503 Internal Server Error", ri.HttpCode)
			ks.Logger.Error("Paniced", "method", ri.Method.Name, "error", r, "stack", string(debug.Stack()))
		}

		labels := MetricLabels{
			"method": ri.Method.Name,
			"status": fmt.Sprintf("%d", ri.HttpCode),
		}

		// FIXME: count bytes transferred as well?
		mType := PrivateMetric
		if ri.Method.IsPublic {
			mType = PublicMetric
		}
		elapsed := uint64(time.Since(ri.Timestamp).Microseconds())
		if ri.Method.Name != "ping" {
			ks.Logger.Info("Finished", "method", ri.Method.Name, "code", ri.HttpCode, "elapsed_us", elapsed)
		}
		ks.MetricsCount("rpc_calls_total", 1, mType, labels)
		if !ri.Method.IsGenerator {
			ks.MetricsSample("rpc_calls_time_us", elapsed, mType, labels)
		}
	}()

	readMsgpack := strings.Contains(ri.Request.Header.Get("Content-Type"), "msgpack")

	var callArgs []reflect.Value

	argsIdx := 2
	if ri.Method.IsGenerator {
		chanType := methodType.In(1)
		channelArg := reflect.MakeChan(reflect.ChanOf(reflect.BothDir, chanType.Elem()), 0)
		callArgs = append(callArgs, channelArg)
		argsIdx = 3
	}

	callArgs = append(callArgs, reflect.ValueOf(ri))

	if len(ri.Method.Args) > 0 {
		pType := methodType.In(argsIdx)
		pVal := reflect.New(pType)
		body, err := io.ReadAll(ri.Request.Body)
		if err == nil && len(body) > 0 {
			var unmarshalErr error
			if readMsgpack {
				unmarshalErr = msgpack.Unmarshal(body, pVal.Interface())
			} else {
				unmarshalErr = json.Unmarshal(body, pVal.Interface())
			}
			if unmarshalErr != nil {
				ri.HttpCode = http.StatusBadRequest
				fmt.Fprintf(os.Stderr, "%+v: err %v", ri.Request, unmarshalErr)
				http.Error(ri.Writer, "400 Bad Request: Invalid Argument Structure", ri.HttpCode)
				return
			}
		}
		callArgs = append(callArgs, pVal.Elem())
	}

	if ri.Method.IsGenerator {
		ks.handleGenerator(ri, methodVal, callArgs)
	} else {
		ks.handleFunction(ri, methodVal, callArgs)
	}
}

func (ks *KettlingarService) setMimeType(ri *RequestInfo, allowSSE bool) (string, bool, bool, bool) {
	accept := ri.Request.Header.Get("Accept")
	if !isExactMimeType(accept) {
		accept = ri.Method.DefaultMime
	}

	useSSE := allowSSE && strings.Contains(accept, "text/event-stream")
	sendJson := useSSE || strings.Contains(accept, "/json")
	sendMsgpack := !sendJson && strings.Contains(accept, "/msgpack")

	ri.Writer.Header().Set("Cache-Control", "no-cache")
	if useSSE {
		ri.Writer.Header().Set("Content-Type", "text/event-stream")
		ri.Writer.Header().Set("Connection", "keep-alive")
	} else if sendMsgpack {
		ri.Writer.Header().Set("Content-Type", "application/msgpack")
	} else if sendJson {
		ri.Writer.Header().Set("Content-Type", "application/json")
	} else {
		ri.Writer.Header().Set("Content-Type", accept)
	}
	return accept, useSSE, sendJson, sendMsgpack
}

func (ks *KettlingarService) handleGenerator(ri *RequestInfo, method reflect.Value, args []reflect.Value) {
	accept, useSSE, sendJson, sendMsgpack := ks.setMimeType(ri, true)

	flusher, _ := ri.Writer.(http.Flusher)
	ctx, cancel := context.WithCancel(ri.Request.Context())
	defer cancel()

	ch := args[0]
	go func() {
		defer ch.Close()
		method.Call(args)
	}()

	first := true
	for {
		chosen, recv, ok := reflect.Select([]reflect.SelectCase{
			{Dir: reflect.SelectRecv, Chan: ch},
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
		})
		if chosen == 1 || !ok {
			if first && ok {
				ri.HttpCode = http.StatusNoContent
				http.Error(ri.Writer, "204 No Content: Channel closed", ri.HttpCode)
			}
			return
		}

		// Guarantee we are working with pointers
		var val interface{}
		if recv.Kind() == reflect.Ptr {
			val = recv.Interface()
		} else {
			ptr := reflect.New(recv.Type())
			ptr.Elem().Set(recv)
			val = ptr.Interface()
		}

		if v, ok := val.(ProgressReporter); ok {
			v.GetProgress().Timestamp = time.Now()
		}
		if useSSE {
			fmt.Fprintf(ri.Writer, "data: %s\n\n", ks.ForgivingJSON(val))
		} else if sendMsgpack {
			msgpack.NewEncoder(ri.Writer).Encode(val)
		} else if sendJson {
			ri.Writer.Write(ks.ForgivingJSON(val))
			ri.Writer.Write([]byte("\n"))
		} else if v, ok := val.(DataRenderer); ok {
			mt, data := v.Render(accept)
			if first {
				if data == nil {
					ri.HttpCode = http.StatusBadRequest
					http.Error(ri.Writer, "400 Bad Request: Accept requested the impossible", ri.HttpCode)
					return
				}
				ri.Writer.Header().Set("Content-Type", mt)
			}
			ri.Writer.Write(data)
		}
		flusher.Flush()
		first = false
		if ri.HttpCode == 0 {
			ri.HttpCode = http.StatusOK
		}
	}
}

func (ks *KettlingarService) handleFunction(ri *RequestInfo, method reflect.Value, args []reflect.Value) {
	accept, _, sendJson, sendMsgpack := ks.setMimeType(ri, false)
	results := method.Call(args)
	if len(results) > 0 {
		recv := results[0]

		// Guarantee we are working with pointers
		var val interface{}
		if recv.Kind() == reflect.Ptr {
			val = recv.Interface()
		} else {
			ptr := reflect.New(recv.Type())
			ptr.Elem().Set(recv)
			val = ptr.Interface()
		}

		if sendMsgpack {
			msgpack.NewEncoder(ri.Writer).Encode(val)
		} else if sendJson {
			ri.Writer.Write(ks.ForgivingJSON(val))
		} else if v, ok := val.(DataRenderer); ok {
			mt, data := v.Render(accept)
			if data != nil {
				ri.Writer.Header().Set("Content-Type", mt)
				ri.Writer.Write(data)
			} else {
				ri.HttpCode = http.StatusBadRequest
				http.Error(ri.Writer, "400 Bad Request: Accept requested the impossible", ri.HttpCode)
				return
			}
		} else {
			ri.HttpCode = http.StatusBadRequest
			http.Error(ri.Writer, "400 Bad Request: Please request JSON or msgpack", ri.HttpCode)
			return
		}
	}
	if ri.HttpCode == 0 {
		ri.HttpCode = http.StatusOK
	}
}

func (ks *KettlingarService) ForgivingJSON(v interface{}) []byte {
	var mpBuf bytes.Buffer

	encoder := msgpack.NewEncoder(&mpBuf)
	encoder.SetSortMapKeys(true)
	if err := encoder.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "uhoh1: %v\n", err)
		return nil
	}

	var jsonBuf bytes.Buffer
	if err := ks.toJson.Convert(&mpBuf, &jsonBuf); err != nil {
		fmt.Fprintf(os.Stderr, "uhoh2: %v\n", err)
		return nil
	}
	return jsonBuf.Bytes()
}

type ProgressReporter interface {
	GetProgress() *ProgressUpdate
}

func (progress *ProgressUpdate) GetProgress() *ProgressUpdate {
	return progress
}

func getIndirectType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func isExactMimeType(accept string) bool {
	re := regexp.MustCompile(`^[a-zA-Z0-9.+_-]+\/[a-zA-Z0-9.+_-]+$`)
	return re.MatchString(accept)
}

func generateSecret() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "="), nil
}
