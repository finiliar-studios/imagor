package server

import (
	"encoding/json"
	"fmt"
	"go.uber.org/zap"
	"net/http"
	"strconv"
)

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	return
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	resJSON(w, GetHealthStats())
	return
}

type Error struct {
	Message string `json:"message,omitempty"`
	Code    int    `json:"status,omitempty"`
}

func (s *Server) panicHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rvr := recover(); rvr != nil {
				err, ok := rvr.(error)
				if !ok {
					err = fmt.Errorf("%v", rvr)
				}
				s.Logger.Error("panic", zap.Error(err))
				w.WriteHeader(http.StatusInternalServerError)
				resJSON(w, Error{
					Message: err.Error(),
					Code:    http.StatusInternalServerError,
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func resJSON(w http.ResponseWriter, v interface{}) {
	buf, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.Write(buf)
	return
}

func pathHandler(method string, handleFuncs map[string]http.HandlerFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != method {
				next.ServeHTTP(w, r)
				return
			}
			if handle, ok := handleFuncs[r.URL.Path]; ok {
				handle(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		})
	}
}
