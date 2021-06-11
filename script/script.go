// Package script provides lua scripting for skipper
package script

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	lua "github.com/yuin/gopher-lua"
	lua_parse "github.com/yuin/gopher-lua/parse"
	"github.com/zalando/skipper/filters"
	"github.com/zalando/skipper/script/base64"

	"github.com/cjoudrey/gluahttp"
	"github.com/cjoudrey/gluaurl"
	gjson "layeh.com/gopher-json"
)

const (
	defaultPoolSize       int = 500 //10000
	defaultScriptPoolSize int = 10
	maxNumberOfScripts        = 100
	genericStateKey           = "generic"
)

type keyedPool struct {
	maxStates int
	// pre created states, which loads all the things we can in advance
	states []*lua.LState
	next   int
	last   int
	mu     sync.Mutex
}

func newKeyedPool(max int) (*keyedPool, error) {
	p := &keyedPool{
		states: make([]*lua.LState, max),
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for i := 0; i < max; i++ {
		state, err := newState()
		if err != nil {
			return nil, err
		}
		p.states[i] = state
	}
	p.next = 0
	p.last = max - 1

	return p, nil
}

func (kp *keyedPool) GetState() (*lua.LState, bool) {
	l, ok := kp.GetStates(1)
	if ok {
		return l[0], true
	}
	return nil, false
}

func (kp *keyedPool) GetStates(n int) ([]*lua.LState, bool) {
	kp.mu.Lock()
	defer kp.mu.Unlock()
	if kp.next < kp.last {
		if kp.last-kp.next < n {
			// not enough states in our pool
			log.Fatalf("not enough states in our pool: kp.next: %d, kp.last: %d, n: %d", kp.next, kp.last, n)
			return nil, false
		}
		last := kp.next + n
		l := kp.states[kp.next:last]
		kp.next = last
		return l, true
	} else {
		log.Fatalf("kp.next: %d, kp.last: %d, n: %d", kp.next, kp.last, n)
		// TODO(sszuecs): do not waste time if we don't need it
		return nil, false
	}
}

func (kp *keyedPool) PutState(state *lua.LState) {
	kp.mu.Lock()
	defer kp.mu.Unlock()
	last := kp.last + 1
	kp.states[last] = state
	kp.last = last
}

type luaScript struct {
	pool *keyedPool
}

// NewLuaScript creates a new filter spec for skipper with default pool size
func NewLuaScript() filters.Spec {
	p, err := newKeyedPool(defaultPoolSize)
	if err != nil {
		log.Fatalf("Failed to create Lua pool: %v", err)
		return nil
	}

	return &luaScript{
		pool: p,
	}
}

// WithPoolSize creates a new filter spec for skipper with global pool size set
func WithPoolSize(bytesAvailable int) filters.Spec {
	return NewLuaScript()

	// memoryLimit := int(bytesAvailable / 10) // let's use only 1/10 for lua

	// // max 1GB
	// if memoryLimit > 1<<30 {
	// 	memoryLimit = 1 << 30
	// }

	// scriptSize := 1024 // assumption (I counted 248B for a request and response filter script, that generates 5B random data)
	// poolSize := memoryLimit / (scriptSize * maxNumberOfScripts)
	// if poolSize > 1000 {
	// 	poolSize = 1000
	// }

	// Init state pool

	// log.Infof("Calculated poolsize: %d", poolSize)
	// ls := &luaScript{
	// 	PoolSize: poolSize,
	// 	// memoryAvailableBytes: memoryLimit,
	// }
	// ls.initPool()
	// return ls
}

// Name returns the name of the filter ("lua")
func (ls *luaScript) Name() string {
	return filters.LuaName
}

// CreateFilter creates the filter
func (ls *luaScript) CreateFilter(config []interface{}) (filters.Filter, error) {
	if len(config) == 0 {
		return nil, filters.ErrInvalidFilterParameters
	}
	src, ok := config[0].(string)
	if !ok {
		return nil, filters.ErrInvalidFilterParameters
	}
	var params []string
	for _, p := range config[1:] {
		ps, ok := p.(string)
		if !ok {
			return nil, filters.ErrInvalidFilterParameters
		}
		params = append(params, ps)
	}

	// hash := fnv.New64()
	// hash.Write([]byte(src))
	// key := fmt.Sprintf("%x", hash.Sum(nil))
	s := &script{
		source:      src,
		globalPool:  ls.pool,
		routeParams: params,
	}
	if err := s.initScript(); err != nil {
		return nil, err
	}
	ls.initPool(s, defaultScriptPoolSize)

	go func() {
		t := time.Tick(time.Second)
		for range t {
			log.Infof("idle pool: %d", atomic.LoadInt32(&s.idlePool))
		}
	}()
	return s, nil
}

func (ls *luaScript) initPool(s *script, max int) error {
	states, ok := ls.pool.GetStates(max)
	if !ok {
		return fmt.Errorf("failed to get %d states", max)
	}
	s.pool = make(chan *lua.LState, max)
	for i := 0; i < max; i++ {
		createScript(states[i], s.proto)
		s.putState(states[i])
	}
	return nil
}

func createScript(L *lua.LState, proto *lua.FunctionProto) (*lua.LState, error) {
	L.Push(L.NewFunctionFromProto(proto))

	err := L.PCall(0, lua.MultRet, nil)
	if err != nil {
		L.Close()
		return nil, err
	}
	return L, nil
}

func newState() (*lua.LState, error) {
	//log.Infof("prepare newState")
	L := lua.NewState()
	L.PreloadModule("base64", base64.Loader)
	L.PreloadModule("http", gluahttp.NewHttpModule(&http.Client{}).Loader)
	L.PreloadModule("url", gluaurl.Loader)
	L.PreloadModule("json", gjson.Loader)
	L.SetGlobal("print", L.NewFunction(printToLog))
	L.SetGlobal("sleep", L.NewFunction(sleep))
	return L, nil
}

// TODO(sszuecs): export metric idlePool
type script struct {
	idlePool int32
	//poolSize                int
	source                  string
	routeParams             []string
	pool                    chan *lua.LState
	globalPool              *keyedPool
	proto                   *lua.FunctionProto
	hasRequest, hasResponse bool
}

func (s *script) newState() (*lua.LState, error) {
	//log.Info("script.newState")
	L, ok := s.globalPool.GetState()
	if !ok {
		return nil, errors.New("Failed to get state from global pool")
	}

	// we should have s.proto by now
	L.Push(L.NewFunctionFromProto(s.proto))

	err := L.PCall(0, lua.MultRet, nil)
	if err != nil {
		L.Close()
		return nil, err
	}
	return L, err
}

func (s *script) initScript() error {
	if err := s.compileSource(); err != nil {
		return err
	}

	// Detect request and response functions
	L, err := s.newState()
	if err != nil {
		return err
	}
	defer L.Close()

	if fn := L.GetGlobal("request"); fn.Type() == lua.LTFunction {
		s.hasRequest = true
	}
	if fn := L.GetGlobal("response"); fn.Type() == lua.LTFunction {
		s.hasResponse = true
	}
	if !s.hasRequest && !s.hasResponse {
		return errors.New("at least one of `request` and `response` function must be present")
	}

	return nil
}

func (s *script) getState() (*lua.LState, error) {
	select {
	case L := <-s.pool:
		if L == nil {
			return nil, errors.New("pool closed")
		}
		atomic.AddInt32(&s.idlePool, -1)
		return L, nil
	default:
		log.Info("create new state")
		return s.newState()
	}
}

func (s *script) putState(L *lua.LState) {
	if s.pool == nil { // pool closed
		L.Close()
		return
	}
	select {
	case s.pool <- L:
		atomic.AddInt32(&s.idlePool, 1)
	default: // pool full, close state
		L.Close()
	}
}

func (s *script) compileSource() error {
	log.Info("compile source")
	// Compile
	var reader io.Reader
	var name string

	if strings.HasSuffix(s.source, ".lua") {
		file, err := os.Open(s.source)
		if err != nil {
			return err
		}
		defer func() {
			if err = file.Close(); err != nil {
				log.Errorf("Failed to close lua file %s: %v", s.source, err)
			}
		}()
		reader = bufio.NewReader(file)
		name = s.source
	} else {
		reader = strings.NewReader(s.source)
		name = "<script>"
	}
	chunk, err := lua_parse.Parse(reader, name)
	if err != nil {
		return err
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return err
	}
	s.proto = proto

	return nil
}

func (s *script) Request(f filters.FilterContext) {
	if s.hasRequest {
		s.runFunc("request", f)
	}
}

func (s *script) Response(f filters.FilterContext) {
	if s.hasResponse {
		s.runFunc("response", f)
	}
}

func (s *script) runFunc(name string, f filters.FilterContext) {
	L, err := s.getState()
	if err != nil {
		log.Errorf("Error obtaining lua environment: %v", err)
		return
	}
	defer s.putState(L)

	pt := L.CreateTable(len(s.routeParams), len(s.routeParams))
	for i, p := range s.routeParams {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) == 1 {
			parts = append(parts, "")
		}
		pt.RawSetString(parts[0], lua.LString(parts[1]))
		pt.RawSetInt(i+1, lua.LString(p))
	}

	err = L.CallByParam(
		lua.P{
			Fn:      L.GetGlobal(name),
			NRet:    0,
			Protect: true,
		},
		s.filterContextAsLuaTable(L, f),
		pt,
	)
	if err != nil {
		log.Errorf("Error calling %s from %s: %v", name, s.source, err)
	}
}

func (s *script) filterContextAsLuaTable(L *lua.LState, f filters.FilterContext) *lua.LTable {
	// this will be passed as parameter to the lua functions
	// add metatable to dynamically access fields in the context
	t := L.CreateTable(0, 0)
	mt := L.CreateTable(0, 1)
	mt.RawSetString("__index", L.NewFunction(getContextValue(f)))
	L.SetMetatable(t, mt)
	return t
}

func printToLog(L *lua.LState) int {
	top := L.GetTop()
	args := make([]interface{}, 0, top)
	for i := 1; i <= top; i++ {
		args = append(args, L.ToStringMeta(L.Get(i)).String())
	}
	log.Print(args...)
	return 0
}

func sleep(L *lua.LState) int {
	time.Sleep(time.Duration(L.CheckInt64(1)) * time.Millisecond)
	return 0
}

func serveRequest(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		t := s.Get(-1)
		r, ok := t.(*lua.LTable)
		if !ok {
			// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
			// s.RaiseError("unsupported type %v, need a table", t.Type())
			// return 0
			s.Push(lua.LString("invalid type, need a table"))
			return 1
		}
		res := &http.Response{}
		r.ForEach(serveTableWalk(s, res))
		f.Serve(res)
		return 0
	}
}

func serveTableWalk(s *lua.LState, res *http.Response) func(lua.LValue, lua.LValue) {
	return func(k, v lua.LValue) {
		sk, ok := k.(lua.LString)
		if !ok {
			// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
			// s.RaiseError("unsupported key type %v, need a string", k.Type())
			return
		}
		switch string(sk) {
		case "status_code":
			n, ok := v.(lua.LNumber)
			if !ok {
				// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
				// s.RaiseError("unsupported status_code type %v, need a number", v.Type())
				return
			}
			res.StatusCode = int(n)

		case "header":
			t, ok := v.(*lua.LTable)
			if !ok {
				// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
				// s.RaiseError("unsupported header type %v, need a table", v.Type())
				return
			}
			h := make(http.Header)
			t.ForEach(serveHeaderWalk(h))
			res.Header = h

		case "body":
			var body []byte
			var err error
			switch v.Type() {
			case lua.LTString:
				data := string(v.(lua.LString))
				body = []byte(data)
			case lua.LTTable:
				body, err = gjson.Encode(v.(*lua.LTable))
				if err != nil {
					// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
					// s.RaiseError("%v", err)
					return
				}
			}
			res.Body = io.NopCloser(bytes.NewBuffer(body))
		}
	}
}

func serveHeaderWalk(h http.Header) func(lua.LValue, lua.LValue) {
	return func(k, v lua.LValue) {
		h.Set(k.String(), v.String())
	}
}

func getContextValue(f filters.FilterContext) func(*lua.LState) int {
	var request, response, state_bag, path_param *lua.LTable
	var serve *lua.LFunction
	return func(s *lua.LState) int {
		key := s.ToString(-1)
		var ret lua.LValue
		switch key {
		case "request":
			// initialize access to request on first use
			if request == nil {
				request = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getRequestValue(f)))
				mt.RawSetString("__newindex", s.NewFunction(setRequestValue(f)))
				s.SetMetatable(request, mt)
			}
			ret = request
		case "response":
			if response == nil {
				response = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getResponseValue(f)))
				mt.RawSetString("__newindex", s.NewFunction(setResponseValue(f)))
				s.SetMetatable(response, mt)
			}
			ret = response
		case "state_bag":
			if state_bag == nil {
				state_bag = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getStateBag(f)))
				mt.RawSetString("__newindex", s.NewFunction(setStateBag(f)))
				s.SetMetatable(state_bag, mt)
			}
			ret = state_bag
		case "path_param":
			if path_param == nil {
				path_param = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 1)
				mt.RawSetString("__index", s.NewFunction(getPathParam(f)))
				s.SetMetatable(path_param, mt)
			}
			ret = path_param
		case "serve":
			if serve == nil {
				serve = s.NewFunction(serveRequest(f))
			}
			ret = serve
		default:
			return 0
		}
		s.Push(ret)
		return 1
	}
}

func getRequestValue(f filters.FilterContext) func(*lua.LState) int {
	var header, cookie, url_query *lua.LTable
	return func(s *lua.LState) int {
		key := s.ToString(-1)
		var ret lua.LValue
		switch key {
		case "header":
			if header == nil {
				header = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 3)
				mt.RawSetString("__index", s.NewFunction(getRequestHeader(f)))
				mt.RawSetString("__newindex", s.NewFunction(setRequestHeader(f)))
				mt.RawSetString("__call", s.NewFunction(iterateRequestHeader(f)))
				s.SetMetatable(header, mt)
			}
			ret = header
		case "cookie":
			if cookie == nil {
				cookie = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 3)
				mt.RawSetString("__index", s.NewFunction(getRequestCookie(f)))
				mt.RawSetString("__newindex", s.NewFunction(unsupported("setting cookie is not supported")))
				mt.RawSetString("__call", s.NewFunction(iterateRequestCookie(f)))
				s.SetMetatable(cookie, mt)
			}
			ret = cookie
		case "outgoing_host":
			ret = lua.LString(f.OutgoingHost())
		case "backend_url":
			ret = lua.LString(f.BackendUrl())
		case "host":
			ret = lua.LString(f.Request().Host)
		case "remote_addr":
			ret = lua.LString(f.Request().RemoteAddr)
		case "content_length":
			ret = lua.LNumber(f.Request().ContentLength)
		case "proto":
			ret = lua.LString(f.Request().Proto)
		case "method":
			ret = lua.LString(f.Request().Method)
		case "url":
			ret = lua.LString(f.Request().URL.String())
		case "url_path":
			ret = lua.LString(f.Request().URL.Path)
		case "url_query":
			if url_query == nil {
				url_query = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 3)
				mt.RawSetString("__index", s.NewFunction(getRequestURLQuery(f)))
				mt.RawSetString("__newindex", s.NewFunction(setRequestURLQuery(f)))
				mt.RawSetString("__call", s.NewFunction(iterateRequestURLQuery(f)))
				s.SetMetatable(url_query, mt)
			}
			ret = url_query
		case "url_raw_query":
			ret = lua.LString(f.Request().URL.RawQuery)
		default:
			return 0
		}
		s.Push(ret)
		return 1
	}
}

func setRequestValue(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		key := s.ToString(-2)
		switch key {
		case "outgoing_host":
			f.SetOutgoingHost(s.ToString(-1))
		case "url":
			u, err := url.Parse(s.ToString(-1))
			if err != nil {
				// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
				// s.RaiseError("%v", err)
				return 0
			}
			f.Request().URL = u
		case "url_path":
			f.Request().URL.Path = s.ToString(-1)
		case "url_raw_query":
			f.Request().URL.RawQuery = s.ToString(-1)
		default:
			// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
			// s.RaiseError("unsupported request field %s", key)
			// do nothing for now
		}
		return 0
	}
}

func getResponseValue(f filters.FilterContext) func(*lua.LState) int {
	var header *lua.LTable
	return func(s *lua.LState) int {
		key := s.ToString(-1)
		var ret lua.LValue
		switch key {
		case "header":
			if header == nil {
				header = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 3)
				mt.RawSetString("__index", s.NewFunction(getResponseHeader(f)))
				mt.RawSetString("__newindex", s.NewFunction(setResponseHeader(f)))
				mt.RawSetString("__call", s.NewFunction(iterateResponseHeader(f)))
				s.SetMetatable(header, mt)
			}
			ret = header
		case "status_code":
			ret = lua.LNumber(f.Response().StatusCode)
		default:
			return 0
		}
		s.Push(ret)
		return 1
	}
}

func setResponseValue(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		key := s.ToString(-2)
		switch key {
		case "status_code":
			v := s.Get(-1)
			n, ok := v.(lua.LNumber)
			if !ok {
				s.RaiseError("unsupported status_code type %v, need a number", v.Type())
				return 0
			}
			f.Response().StatusCode = int(n)
		default:
			s.RaiseError("unsupported response field %s", key)
		}
		return 0
	}
}

func getStateBag(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		fld := s.ToString(-1)
		res, ok := f.StateBag()[fld]
		if !ok {
			return 0
		}
		switch res := res.(type) {
		case string:
			s.Push(lua.LString(res))
		case int:
			s.Push(lua.LNumber(res))
		case int64:
			s.Push(lua.LNumber(res))
		case float64:
			s.Push(lua.LNumber(res))
		default:
			return 0
		}
		return 1
	}
}

func setStateBag(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		fld := s.ToString(-2)
		val := s.Get(-1)
		var res interface{}
		switch val.Type() {
		case lua.LTString:
			res = string(val.(lua.LString))
		case lua.LTNumber:
			res = float64(val.(lua.LNumber))
		default:
			// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
			// s.RaiseError("unsupported state bag value type %v, need a string or a number", val.Type())
			return 0
		}
		f.StateBag()[fld] = res
		return 0
	}
}

func getRequestHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		hdr := s.ToString(-1)
		res := f.Request().Header.Get(hdr)
		// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
		// if res != "" {
		//	s.Push(lua.LString(res))
		//	return 1
		// }
		// return 0
		s.Push(lua.LString(res))
		return 1
	}
}

func setRequestHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		lv := s.Get(-1)
		hdr := s.ToString(-2)
		switch lv.Type() {
		case lua.LTNil:
			f.Request().Header.Del(hdr)
		case lua.LTString:
			str := string(lv.(lua.LString))
			if str == "" {
				f.Request().Header.Del(hdr)
			} else {
				f.Request().Header.Set(hdr, str)
			}
		default:
			val := s.ToString(-1)
			f.Request().Header.Set(hdr, val)
		}
		return 0
	}
}

func iterateRequestHeader(f filters.FilterContext) func(*lua.LState) int {
	// https://www.lua.org/pil/7.2.html
	return func(s *lua.LState) int {
		s.Push(s.NewFunction(nextHeader(f.Request().Header)))
		return 1
	}
}

func getRequestCookie(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		k := s.ToString(-1)
		c, err := f.Request().Cookie(k)
		if err == nil {
			s.Push(lua.LString(c.Value))
			return 1
		}
		return 0
	}
}

func iterateRequestCookie(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		s.Push(s.NewFunction(nextCookie(f.Request().Cookies())))
		return 1
	}
}

func nextCookie(cookies []*http.Cookie) func(*lua.LState) int {
	return func(s *lua.LState) int {
		if len(cookies) > 0 {
			c := cookies[0]
			s.Push(lua.LString(c.Name))
			s.Push(lua.LString(c.Value))
			cookies[0] = nil // mind peace
			cookies = cookies[1:]
			return 2
		}
		return 0
	}
}

func getResponseHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		hdr := s.ToString(-1)
		res := f.Response().Header.Get(hdr)
		// TODO(sszuecs): https://github.com/zalando/skipper/issues/1487
		// if res != "" {
		//	s.Push(lua.LString(res))
		//	return 1
		// }
		// return 0
		s.Push(lua.LString(res))
		return 1
	}
}

func setResponseHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		lv := s.Get(-1)
		hdr := s.ToString(-2)
		switch lv.Type() {
		case lua.LTNil:
			f.Response().Header.Del(hdr)
		case lua.LTString:
			str := string(lv.(lua.LString))
			if str == "" {
				f.Response().Header.Del(hdr)
			} else {
				f.Response().Header.Set(hdr, str)
			}
		default:
			val := s.ToString(-1)
			f.Response().Header.Set(hdr, val)
		}
		return 0
	}
}

func iterateResponseHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		s.Push(s.NewFunction(nextHeader(f.Response().Header)))
		return 1
	}
}

func nextHeader(h http.Header) func(*lua.LState) int {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return func(s *lua.LState) int {
		if len(keys) > 0 {
			k := keys[0]
			s.Push(lua.LString(k))
			s.Push(lua.LString(h.Get(k)))
			keys[0] = "" // mind peace
			keys = keys[1:]
			return 2
		}
		return 0
	}
}

func getRequestURLQuery(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		k := s.ToString(-1)
		res := f.Request().URL.Query().Get(k)
		if res != "" {
			s.Push(lua.LString(res))
			return 1
		}
		return 0
	}
}

func setRequestURLQuery(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		lv := s.Get(-1)
		k := s.ToString(-2)
		q := f.Request().URL.Query()
		switch lv.Type() {
		case lua.LTNil:
			q.Del(k)
		case lua.LTString:
			str := string(lv.(lua.LString))
			if str == "" {
				q.Del(k)
			} else {
				q.Set(k, str)
			}
		default:
			val := s.ToString(-1)
			q.Set(k, val)
		}
		f.Request().URL.RawQuery = q.Encode()
		return 0
	}
}

func iterateRequestURLQuery(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		s.Push(s.NewFunction(nextQuery(f.Request().URL.Query())))
		return 1
	}
}

func nextQuery(v url.Values) func(*lua.LState) int {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	return func(s *lua.LState) int {
		if len(keys) > 0 {
			k := keys[0]
			s.Push(lua.LString(k))
			s.Push(lua.LString(v.Get(k)))
			keys[0] = "" // mind peace
			keys = keys[1:]
			return 2
		}
		return 0
	}
}

func getPathParam(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		n := s.ToString(-1)
		p := f.PathParam(n)
		if p != "" {
			s.Push(lua.LString(p))
			return 1
		}
		return 0
	}
}

func unsupported(message string) func(*lua.LState) int {
	return func(s *lua.LState) int {
		s.RaiseError(message)
		return 0
	}
}
