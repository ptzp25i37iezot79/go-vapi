package vapi

import (
	"sync"
	"reflect"
	"unicode/utf8"
	"unicode"
	"fmt"
	"net/http"
	"strings"
	"github.com/riftbit/httprouterc"
	"context"
	"github.com/justinas/alice"
	"github.com/gorilla/schema"
)


var (
	Server *ApiServer
	// Precompute the reflect.Type of error and http.Request
	typeOfError   = reflect.TypeOf((*error)(nil)).Elem()
	typeOfRequest = reflect.TypeOf((*http.Request)(nil)).Elem()
	baseMiddleWares = alice.New()
	schemaDecoder = schema.NewDecoder()
)

// serviceMap is a registry for services.
type serviceMap struct {
	mutex    sync.Mutex
	services map[string]*service
}

type service struct {
	name     string                    // name of service
	rcvr     reflect.Value             // receiver of methods for the service
	rcvrType reflect.Type              // type of the receiver
	methods  map[string]*serviceMethod // registered methods
}

type serviceMethod struct {
	method    reflect.Method // receiver method
	argsType  reflect.Type   // type of the request argument
	replyType reflect.Type   // type of the response argument
}

type ApiServer struct {
	services *serviceMap
	router *httprouterc.Router
}



func (as *ApiServer) AddRoute(method string, uri string, h http.Handler) {
	as.router.Handle(method, uri, wrapHandler(h))
}

func (as *ApiServer) AddRouteF(method string, uri string, h http.HandlerFunc) {
	as.router.Handle(method, uri, Wrap(h))
}

func (as *ApiServer) GetRouter() *httprouterc.Router {
	return as.router
}


func (as *ApiServer) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, as.router)
}


func Wrap(h http.HandlerFunc, m ...alice.Constructor ) httprouterc.Handle {
	b := baseMiddleWares.Extend(alice.New(m...))
	return wrapHandler(b.ThenFunc(h))
}

func wrapHandler(h http.Handler) httprouterc.Handle {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(ctx)
		h.ServeHTTP(w, r)
	}
}


func ApiHandler(w http.ResponseWriter, r *http.Request) {

	if strings.Contains(r.Context().Value("method").(string), ".") != true {
		WritePureError(w, 404, "api: Method not found: "+r.Context().Value("method").(string))
		return
	}

	partsMethod := strings.SplitN(r.Context().Value("method").(string), ".", 2)
	if len(partsMethod) < 2  {
		WritePureError(w, 404, "api: Method not found: "+r.Context().Value("method").(string))
		return
	}
	partsMethod[1] = strings.Title(partsMethod[1])
	method := strings.Join(partsMethod, ".")

	ctx := context.WithValue(r.Context(), "method", method)
	r = r.WithContext(ctx)

	if Server.HasMethod(method) == false {
		WritePureError(w, 404, "api: Method not found: "+method)
		return
	} else {
		Server.ServeHTTP(w, r)
	}
}

func Initialize(baseURL string, middlewares ...alice.Constructor) {
	Server = newApiServer(baseURL, middlewares...)
}


// NewServer returns a new RPC server.
func newApiServer(baseURL string, middlewares ...alice.Constructor) *ApiServer {
	router := httprouterc.New()
	router.GET(baseURL+"/:method", Wrap(ApiHandler, middlewares...))
	router.POST(baseURL+"/:method", Wrap(ApiHandler, middlewares...))

	return &ApiServer{
		services: new(serviceMap),
		router: router,
	}
}


// HasMethod returns true if the given method is registered.
//
// The method uses a dotted notation as in "Service.Method".
func (s *ApiServer) HasMethod(method string) bool {
	if _, _, err := s.services.get(method); err == nil {
		return true
	}
	return false
}

// RegisterService adds a new service to the server.
//
// The name parameter is optional: if empty it will be inferred from
// the receiver type name.
//
// Methods from the receiver will be extracted if these rules are satisfied:
//
//    - The receiver is exported (begins with an upper case letter) or local
//      (defined in the package registering the service).
//    - The method name is exported.
//    - The method has three arguments: *http.Request, *args, *reply.
//    - All three arguments are pointers.
//    - The second and third arguments are exported or local.
//    - The method has return type error.
//
// All other methods are ignored.
func (s *ApiServer) RegisterService(receiver interface{}, name string) error {
	return s.services.register(receiver, name)
}


// get returns a registered service given a method name.
//
// The method name uses a dotted notation as in "Service.Method".
func (m *serviceMap) get(method string) (*service, *serviceMethod, error) {
	parts := strings.Split(method, ".")
	if len(parts) != 2 {
		err := fmt.Errorf("api: service/method request ill-formed: %q", method)
		return nil, nil, err
	}
	m.mutex.Lock()
	service := m.services[parts[0]]
	m.mutex.Unlock()
	if service == nil {
		err := fmt.Errorf("api: can't find service %q", method)
		return nil, nil, err
	}
	serviceMethod := service.methods[parts[1]]
	if serviceMethod == nil {
		err := fmt.Errorf("api: can't find method %q", method)
		return nil, nil, err
	}
	return service, serviceMethod, nil
}



// get returns a registered service given a method name.
//
// The method name uses a dotted notation as in "Service.Method".
func (m *serviceMap) GetAll() (map[string]*service, error) {
	m.mutex.Lock()
	service := m.services
	m.mutex.Unlock()
	return service, nil
}


// register adds a new service using reflection to extract its methods.
func (m *serviceMap) register(rcvr interface{}, name string) error {
	// Setup service.
	s := &service{
		name:     name,
		rcvr:     reflect.ValueOf(rcvr),
		rcvrType: reflect.TypeOf(rcvr),
		methods:  make(map[string]*serviceMethod),
	}
	if name == "" {
		s.name = reflect.Indirect(s.rcvr).Type().Name()
		if !isExported(s.name) {
			return fmt.Errorf("api: type %q is not exported", s.name)
		}
	}
	if s.name == "" {
		return fmt.Errorf("api: no service name for type %q",
			s.rcvrType.String())
	}
	// Setup methods.
	for i := 0; i < s.rcvrType.NumMethod(); i++ {
		method := s.rcvrType.Method(i)

		mtype := method.Type
		// Method must be exported.
		if method.PkgPath != "" {
			continue
		}
		// Method needs four ins: receiver, ps httprouter.Params, *http.Request, *args, *reply.
		if mtype.NumIn() != 4 {
			continue
		}

		// First argument must be a pointer and must be http.Request.
		reqType := mtype.In(1)
		if reqType.Kind() != reflect.Ptr || reqType.Elem() != typeOfRequest {
			continue
		}
		// Second argument must be a pointer and must be exported.
		args := mtype.In(2)
		if args.Kind() != reflect.Ptr || !isExportedOrBuiltin(args) {
			continue
		}
		// Third argument must be a pointer and must be exported.
		reply := mtype.In(3)
		if reply.Kind() != reflect.Ptr || !isExportedOrBuiltin(reply) {
			continue
		}
		// Method needs one out: error.
		if mtype.NumOut() != 1 {
			continue
		}
		if returnType := mtype.Out(0); returnType != typeOfError {
			continue
		}

		s.methods[method.Name] = &serviceMethod{
			method:    method,
			argsType:  args.Elem(),
			replyType: reply.Elem(),
		}
	}
	if len(s.methods) == 0 {
		return fmt.Errorf("api: %q has no exported methods of suitable type",
			s.name)
	}
	// Add to the map.
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.services == nil {
		m.services = make(map[string]*service)
	} else if _, ok := m.services[s.name]; ok {
		return fmt.Errorf("api: service already defined: %q", s.name)
	}
	m.services[s.name] = s
	return nil
}

// isExported returns true of a string is an exported (upper case) name.
func isExported(name string) bool {
	runez, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(runez)
}

// isExportedOrBuiltin returns true if a type is exported or a builtin.
func isExportedOrBuiltin(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return isExported(t.Name()) || t.PkgPath() == ""
}


// ServeHTTP
func (s *ApiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "GET" {
		WritePureError(w, 405, "api: POST or GET method required, received "+r.Method)
		return
	}

	var selectedCodec codecServerResponseInterface
	if strings.HasSuffix(r.URL.Query().Get("format"), "xml") {
		selectedCodec = &serverResponseXML{}
	} else {
		selectedCodec = &serverResponseJSON{}
	}

	var codec CodecRequest
	// Create a new codec request.
	codecReq := codec.NewRequest(r, selectedCodec)
	// Get service method to be called.
	method, errMethod := codecReq.Method()
	if errMethod != nil {
		WritePureError(w, 400, errMethod.Error())
		return
	}

	serviceSpec, methodSpec, errGet := s.services.get(method)
	if errGet != nil {
		codecReq.Responser.WriteError(w, 400, errGet)
		return
	}

	// Decode the args.
	args := reflect.New(methodSpec.argsType)
	if errRead := codecReq.ReadRequest(args.Interface()); errRead != nil {
		codecReq.Responser.WriteError(w, 400, errRead)
		return
	}

	// Call the service method.
	reply := reflect.New(methodSpec.replyType)
	errValue := methodSpec.method.Func.Call([]reflect.Value{
		serviceSpec.rcvr,
		reflect.ValueOf(r),
		args,
		reply,
	})

	// Cast the result to error if needed.
	var errResult error
	errInter := errValue[0].Interface()
	if errInter != nil {
		errResult = errInter.(error)
	}

	// Prevents Internet Explorer from MIME-sniffing a response away
	// from the declared content-type
	w.Header().Set("x-content-type-options", "nosniff")
	// Encode the response.
	if errResult == nil {
		codecReq.Responser.WriteResponse(w, reply.Interface())
	} else {
		codecReq.Responser.WriteError(w, 400, errResult)
	}
}


func WritePureError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, msg)
}
