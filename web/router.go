package web

import (
	"fmt"
	"github.com/efigence/go-mon"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type WebBackend struct {
	l         *zap.SugaredLogger
	al        *zap.SugaredLogger
	r         *gin.Engine
	subRouter SubdomainRouter
	cfg       *Config
}

type Config struct {
	Logger       *zap.SugaredLogger `yaml:"-"`
	AccessLogger *zap.SugaredLogger `yaml:"-"`
	ListenAddr   string             `yaml:"listen_addr"`
}

type SubdomainRouter struct {
	subdomains map[string]http.Handler
	def        http.Handler
}

func (s SubdomainRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	domainParts := strings.Split(r.Host, ".")

	if mux := s.subdomains[domainParts[0]]; mux != nil {
		mux.ServeHTTP(w, r)
	} else {
		http.Error(w, "Not found", 404)
	}
}

func New(cfg Config, webFS fs.FS) (backend *WebBackend, err error) {
	if cfg.Logger == nil {
		panic("missing logger")
	}
	if len(cfg.ListenAddr) == 0 {
		panic("missing listen addr")
	}
	w := WebBackend{
		l:         cfg.Logger,
		al:        cfg.AccessLogger,
		subRouter: SubdomainRouter{subdomains: map[string]http.Handler{}},
		cfg:       &cfg,
	}
	if cfg.AccessLogger == nil {
		w.al = w.l //.Named("accesslog")
	}
	r := gin.New()
	w.r = r
	w.subRouter.def = r
	gin.SetMode(gin.ReleaseMode)
	t, err := template.ParseFS(webFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("error loading templates: %s", err)
	}
	r.SetHTMLTemplate(t)
	// for zap logging
	r.Use(ginzap.GinzapWithConfig(w.al.Desugar(), &ginzap.Config{
		TimeFormat: time.RFC3339,
		UTC:        false,
		SkipPaths:  []string{"/_status/health", "/_status/metrics"},
	}))
	//r.Use(ginzap.RecoveryWithZap(w.al.Desugar(), true))
	// basic logging to stdout
	//r.Use(gin.LoggerWithWriter(os.Stdout))
	r.Use(gin.Recovery())

	// monitoring endpoints
	r.GET("/_status/health", gin.WrapF(mon.HandleHealthcheck))
	r.HEAD("/_status/health", gin.WrapF(mon.HandleHealthcheck))
	r.GET("/_status/metrics", gin.WrapF(mon.HandleMetrics))
	defer mon.GlobalStatus.Update(mon.StatusOk, "ok")
	// healthcheckHandler, haproxyStatus := mon.HandleHealthchecksHaproxy()
	// r.GET("/_status/metrics", gin.WrapF(healthcheckHandler))

	httpFS := http.FileServer(http.FS(webFS))
	r.GET("/s/*filepath", func(c *gin.Context) {
		// content is embedded under static/ dir
		p := strings.Replace(c.Request.URL.Path, "/s/", "/static/", -1)
		c.Request.URL.Path = p
		//c.Header("Cache-Control", "public, max-age=3600, immutable")
		httpFS.ServeHTTP(c.Writer, c.Request)
	})
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.tmpl", gin.H{
			"title": c.Request.RemoteAddr,
		})
	})
	r.NoRoute(func(c *gin.Context) {
		c.HTML(http.StatusNotFound, "404.tmpl", gin.H{
			"notfound": c.Request.URL.Path,
		})
	})
	mon.GlobalStatus.Update(mon.Ok, "ok")
	return &w, nil
}

func (b *WebBackend) AddSubdomainRouter(subdomain string, r http.Handler) error {
	if _, ok := b.subRouter.subdomains[subdomain]; !ok {
		b.subRouter.subdomains[subdomain] = r
		return nil
	} else {
		return fmt.Errorf("tried to register duplicate domain")
	}
}

func (b *WebBackend) RunHTTP() error {
	b.l.Infof("listening on %s", b.cfg.ListenAddr)
	return http.ListenAndServe(b.cfg.ListenAddr, b.subRouter)
}

func (b *WebBackend) RunUnix(file string, remove bool) error {
	listener, err := net.Listen("unix", file)
	if err != nil {
		return err
	}
	defer listener.Close()
	if remove {
		defer os.Remove(file)
	}

	return http.Serve(listener, b.subRouter)
}
