package kettlingar

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

// levelTrace is the sub-debug level the RPC client traces at. It matches the
// pagekite-go logtee TRACE level (slog.LevelDebug - 4) without taking a
// dependency on that package, so client wiring and per-call detail are only
// emitted when the sink is opened all the way down to TRACE.
const levelTrace = slog.LevelDebug - 4

// MakeClient populates a struct of function fields with RPC implementations.
func MakeClient(name, url string, clientPtr interface{}) {
	val := reflect.ValueOf(clientPtr).Elem()
	typ := val.Type()

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Func {
			continue
		}

		methodName := dashedName(field.Name)
		endpoint := fmt.Sprintf("%s/%s", strings.TrimSuffix(url, "/"), methodName)
		slog.Default().Log(context.Background(), levelTrace,
			name+": client method wired", "service", name, "method", methodName, "endpoint", endpoint)

		fn := func(args []reflect.Value) (results []reflect.Value) {
			// 1. Determine if this is a streaming call
			var isStreaming bool
			var reqVal interface{}
			var outChan reflect.Value

			if len(args) > 0 && args[0].Kind() == reflect.Chan {
				isStreaming = true
				outChan = args[0]
				reqVal = args[1].Interface()
			} else {
				reqVal = args[0].Interface()
			}

			payload, _ := msgpack.Marshal(reqVal)

			req, _ := http.NewRequest("POST", endpoint, bytes.NewBuffer(payload))
			req.Header.Set("Content-Type", "application/msgpack")
			req.Header.Set("Accept", "application/msgpack")

			log := slog.Default()
			log.Log(context.Background(), levelTrace, name+": rpc request",
				"method", methodName, "endpoint", endpoint, "streaming", isStreaming, "req_bytes", len(payload))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Log(context.Background(), levelTrace, name+": rpc request failed",
					"method", methodName, "endpoint", endpoint, "err", err)
				panic(fmt.Errorf("%s: %s call failed: %w", name, methodName, err))
			}
			log.Log(context.Background(), levelTrace, name+": rpc response",
				"method", methodName, "status", resp.StatusCode, "streaming", isStreaming)

			if isStreaming {
				go func() {
					defer resp.Body.Close()
					defer outChan.Close()
					dec := msgpack.NewDecoder(resp.Body)
					var n int
					for {
						elem := reflect.New(outChan.Type().Elem())
						if err := dec.Decode(elem.Interface()); err == io.EOF {
							break
						} else if err != nil {
							continue
						}
						n++
						outChan.Send(elem.Elem())
					}
					log.Log(context.Background(), levelTrace, name+": rpc stream closed",
						"method", methodName, "frames", n)
				}()
				return nil
			}

			defer resp.Body.Close()
			outTyp := field.Type.Out(0)
			outVal := reflect.New(outTyp)
			msgpack.NewDecoder(resp.Body).Decode(outVal.Interface())
			return []reflect.Value{outVal.Elem()}
		}

		val.Field(i).Set(reflect.MakeFunc(field.Type, fn))
	}
}
