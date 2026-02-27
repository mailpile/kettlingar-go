package kettlingar

import "time"

type DefaultMethods struct{}

type PingArguments struct {
	Methods bool `default:"false"`
}

type PingResponse struct {
	Timestamp time.Time    `msgpack:"timestamp" json:"timestamp"`
	Version   string       `msgpack:"version" json:"version"`
	Methods   []MethodDesc `msgpack:"methods" json:"methods"`
}

func (d *DefaultMethods) GetDocs() map[string]MethodDesc {
	return map[string]MethodDesc{
		"ping": {
			Help: "Checks server status and returns the API manifest",
			Docs: "This endpoint provides the complete list of registered methods",
		},
	}
}

//func (d *DefaultMethods) PublicApiAsplode(ri *RequestInfo) PingResponse {
//       panic("OMG, I asploded")
//}

func (d *DefaultMethods) PublicApiPing(ri *RequestInfo, args PingArguments) PingResponse {
	var methods []MethodDesc
	if args.Methods {
		methods = ri.Service.GetApiManifest()
		if !ri.IsAuthed {
			pubMethods := make([]MethodDesc, len(methods))
			pubMethods = pubMethods[:0]
			for _, mDesc := range methods {
				if mDesc.IsPublic {
					pubMethods = append(pubMethods, mDesc)
				}
			}
			methods = pubMethods
		}
	}
	return PingResponse{
		Timestamp: time.Now(),
		Version:   ri.Service.Version,
		Methods:   methods,
	}
}
