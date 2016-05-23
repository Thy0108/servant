package server

import (
	"servant/conf"
	"net/http"
	"sync/atomic"
	"time"
	"regexp"
	"fmt"
	"os"
)

const ServantErrHeader = "X-Servant-Err"

type Server struct {
	config          *conf.Config
	resources       map[string]HandlerFactory
	nextSessionId   uint64
}

type Session struct {
	id       uint64
	config   *conf.Config
	resource, group, item, tail string
	username string
	resp     http.ResponseWriter
	req      *http.Request
}

type ServantError struct {
	HttpCode   int
	Message    string
	//Error      error
}

func NewServantError(code int, format string, v ...interface{}) ServantError {
	return ServantError {
		HttpCode: code,
		Message: fmt.Sprintf(format, v...),
	}
}

func (self ServantError) Error() string {
	return fmt.Sprintf("%d: %s", self.HttpCode, self.Message)
}

func NewServer(config *conf.Config) *Server {
	ret := &Server {
		config:         config,
		nextSessionId:  0,
		resources:      make(map[string]HandlerFactory),
	}
	if config.Log != "" {
		file, err := os.OpenFile(config.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0664)
		if err == nil {
			logger.SetOutput(file)
		} else {
			logger.Printf("can not open log file %s", config.Log)
		}
	}
	ret.resources["commands"] = NewCommandServer
	ret.resources["files"] = NewFileServer
	ret.resources["databases"] = NewDatabaseServer
	return ret
}

func (self *Server) newSession(resp http.ResponseWriter, req *http.Request) *Session {
	resource, group, item, tail := parseUriPath(req.URL.Path)
	sess := Session {
		id:       atomic.AddUint64(&(self.nextSessionId), 1),
		config:   self.config,
		req:      req,
		resp:     resp,
		resource: resource,
		group:    group,
		item:     item,
		tail:     tail,
	}
	return &sess
}


var uriRe, _ = regexp.Compile(`^/([a-zA-Z]\w*)/([a-zA-Z]\w*)/([a-zA-Z]\w*)((?:/.*)?)$`)
func parseUriPath(path string) (resource, group, item, tail string) {
	m := uriRe.FindStringSubmatch(path)
	if len(m) != 5 {
		return "", "", "", ""
	}
	resource, group, item, tail = m[1], m[2], m[3], m[4]
	return
}

var paramRe, _ = regexp.Compile(`\${[a-zA-Z_]\w*(?:\.[a-zA-Z_]\w*)?}`)
var paramNameRe, _ = regexp.Compile(`^[a-zA-Z]\w+$`)

func requestParams(req *http.Request) func(string)string {
	// ${aaa} ${foo.bar} ${_env.PATH}
	q := req.URL.Query() // should be out of the closure, to avoid parse query many times
	return func(k string) string {
		if v, ok := GetGlobalParam(k); ok {
			return v
		}
		if req != nil {
			if ok := paramNameRe.MatchString(k); ok {
				return q.Get(k)
			}
		}
		return ""
	}
}

func globalParam() func(string)string {
	return func(k string) string {
		v, _ := GetGlobalParam(k)
		return v
	}
}

func (self *Server) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	sess := self.newSession(resp, req)
	sess.info("+ %s %s %s", req.RemoteAddr, req.Method, req.URL.Path)
	username, err := sess.auth()
	if err != nil {
		sess.ErrorEnd(http.StatusForbidden, "auth failed: %s", err)
		return
	}
	sess.username = username
	if ! sess.checkPermission() {
		sess.ErrorEnd(http.StatusForbidden, "access of %s forbidden", req.URL.Path)
		return
	}
	handlerFactory, ok := self.resources[sess.resource]
	if !ok {
		sess.ErrorEnd(http.StatusNotFound, "unknown resource")
		return
	}
	handlerFactory(sess).serve()
}

type Handler interface {
	serve()
}

type HandlerFactory func(sess *Session) Handler


func (self *Session) ErrorEnd(code int, format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	self.warn("- " + msg)
	self.resp.Header().Set(ServantErrHeader, msg)
	self.resp.WriteHeader(code)
}

func (self *Session) BadEnd(format string, v ...interface{}) {
	self.warn("- " + format, v...)
}

func (self *Session) GoodEnd(format string, v ...interface{}) {
	self.info("- " + format, v...)
}

func (self *Session) UserConfig() *conf.User {
	ret, _ := self.config.Users[self.username]
	return ret
}

func (self *Server) StartDaemons() {
	for name, conf := range(self.config.Daemons) {
		go RunDaemon(name, conf)
	}
}

func (self *Server) StartTimers() {
	for name, conf := range(self.config.Timers) {
		go RunTimer(name, conf)
	}
}

func (self *Server) Run() error {
	s := &http.Server{
		Addr:           self.config.Server.Listen,
		Handler:        self,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 8192,
	}
	self.StartDaemons()
	self.StartTimers()
	return s.ListenAndServe()
}

