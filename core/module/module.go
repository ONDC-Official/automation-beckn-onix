package module

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/beckn-one/beckn-onix/core/module/handler"
	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

// Config represents the configuration for a module.
type Config struct {
	Name    string `yaml:"name"`
	Path    string `yaml:"path"`
	Handler handler.Config
}

// Provider represents a function that initializes an HTTP handler using a PluginManager.
type Provider func(ctx context.Context, mgr handler.PluginManager, cfg *handler.Config, moduleName string) (http.Handler, error)

// handlerProviders maintains a mapping of handler types to their respective providers.
var handlerProviders = map[handler.Type]Provider{
	handler.HandlerTypeStd: handler.NewStdHandler,
}

// Register initializes and registers handlers based on the provided configuration.
// It iterates over the module configurations, retrieves appropriate handler providers,
// and registers the handlers with the HTTP multiplexer.
func Register(ctx context.Context, mCfgs []Config, mux *http.ServeMux, mgr handler.PluginManager) error {

	mux.Handle("/health", http.HandlerFunc(handler.HealthHandler))

	log.Debugf(ctx, "Registering modules with config: %#v", mCfgs)
	// Iterate over the handlers in the configuration.
	for _, c := range mCfgs {
		rmp, ok := handlerProviders[c.Handler.Type]
		if !ok {
			return fmt.Errorf("invalid module : %s", c.Name)
		}
		h, err := rmp(ctx, mgr, &c.Handler, c.Name)
		if err != nil {
			return fmt.Errorf("%s : %w", c.Name, err)
		}
		h, err = addMiddleware(ctx, mgr, h, &c.Handler)
		if err != nil {
			return fmt.Errorf("failed to add middleware: %w", err)

		}
		h = moduleCtxMiddleware(c.Name, h)
		h = PostResponseMiddleware()(h)
		log.Debugf(ctx, "Registering handler %s, of type %s @ %s", c.Name, c.Handler.Type, c.Path)
		mux.Handle(c.Path, h)
	}
	return nil
}

// addMiddleware applies middleware plugins to the provided handler in reverse order.
// It retrieves middleware instances from the plugin manager and chains them to the handler.
func addMiddleware(ctx context.Context, mgr handler.PluginManager, handler http.Handler, hCfg *handler.Config) (http.Handler, error) {
	mws := hCfg.Plugins.Middleware
	log.Debugf(ctx, "Applying %d middleware(s) to the handler", len(mws))
	// Apply the middleware in reverse order.
	for i := len(mws) - 1; i >= 0; i-- {
		log.Debugf(ctx, "Loading middleware: %s", mws[i].ID)
		mw, err := mgr.Middleware(ctx, &mws[i])
		if err != nil {
			log.Errorf(ctx, err, "Failed to load middleware %s: %v", mws[i].ID, err)
			return nil, fmt.Errorf("failed to load middleware %s: %w", mws[i].ID, err)
		}
		// Apply the middleware to the handler.
		handler = mw(handler)
		log.Debugf(ctx, "Applied middleware: %s", mws[i].ID)
	}

	log.Debugf(ctx, "Middleware chain setup completed")
	return handler, nil
}

func moduleCtxMiddleware(moduleName string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), model.ContextKeyModuleID, moduleName)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PostResponseMiddleware runs the hooks added with handler.RegisterPostResponseHook
// after the response has been sent to the caller.
func PostResponseMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var hooks []handler.PostResponseHook

			// The request context is canceled when the caller hangs up. That should
			// still stop the handler while it is working, but it must not stop the
			// hooks, which run after the caller already has its response. So the
			// handler gets a context that follows the caller only until it returns.
			ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
			defer cancel()
			stopFollowingCaller := context.AfterFunc(r.Context(), cancel)

			// store hook list pointer in request context
			ctx = context.WithValue(ctx, handler.PostResponseKey{}, &hooks)
			r = r.WithContext(ctx)

			pw := &postResponseWriter{ResponseWriter: w, hooks: &hooks}
			next.ServeHTTP(pw, r)
			stopFollowingCaller()

			if len(hooks) == 0 {
				return
			}
			// Send the response now, so the caller does not wait for the hooks.
			pw.send()

			for _, hook := range hooks {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Errorf(ctx, nil, "post-response hook panic: %v", r)
						}
					}()
					hook()
				}()
			}
		})
	}
}

// postResponseWriter holds back a response that is written after a hook was
// registered, so it can be sent complete (with Content-Length) before the hooks
// run. Without this the response stays in the server's buffer until the hooks
// finish. Responses written with no hook registered, such as proxy responses,
// pass straight through.
type postResponseWriter struct {
	http.ResponseWriter
	hooks     *[]handler.PostResponseHook
	decided   bool
	buffering bool
	status    int
	buf       bytes.Buffer
}

func (w *postResponseWriter) decide() {
	if !w.decided {
		w.decided = true
		w.buffering = len(*w.hooks) > 0
	}
}

func (w *postResponseWriter) WriteHeader(code int) {
	w.decide()
	if !w.buffering {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status == 0 {
		w.status = code
	}
}

func (w *postResponseWriter) Write(p []byte) (int, error) {
	w.decide()
	if !w.buffering {
		return w.ResponseWriter.Write(p)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(p)
}

// Flush passes through, except while a response is being held back.
func (w *postResponseWriter) Flush() {
	w.decide()
	if !w.buffering {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *postResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// send writes out a held back response and pushes it to the caller.
func (w *postResponseWriter) send() {
	if w.buffering {
		h := w.ResponseWriter.Header()
		if h.Get("Content-Length") == "" {
			h.Set("Content-Length", strconv.Itoa(w.buf.Len()))
		}
		// The connection stays busy until the hooks finish. Ask the caller not to
		// reuse it, so its next request does not wait behind the hooks.
		h.Set("Connection", "close")
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
