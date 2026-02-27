package kettlingar

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

// MakeClient populates a struct of function fields with RPC implementations.
func MakeClient(name, url string, clientPtr interface{}) {
	val := reflect.ValueOf(clientPtr).Elem()
	typ := val.Type()

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Func {
			continue
		}

		methodName := strings.ToLower(field.Name)
		endpoint := fmt.Sprintf("%s/%s", strings.TrimSuffix(url, "/"), methodName)

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

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				panic(fmt.Errorf("kettlingar: %s call failed: %w", methodName, err))
			}

			if isStreaming {
				go func() {
					defer resp.Body.Close()
					defer outChan.Close()
					dec := msgpack.NewDecoder(resp.Body)
					for {
						elem := reflect.New(outChan.Type().Elem())
						if err := dec.Decode(elem.Interface()); err == io.EOF {
							break
						} else if err != nil {
							continue
						}
						outChan.Send(elem.Elem())
					}
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
