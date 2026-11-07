package webapi

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/httpauth"
)

// logRequests writes one line per /api request: method, path, the query
// parameter NAMES plus the cost-shaping values (since_minutes, limit), status,
// response bytes and duration. Without it there is no way to tell which
// request drove the query process into its memory limit. Filter values and
// tokens are never logged.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		key := "-" // stays "-" when auth rejected the request
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), keySlot{}, &key)))

		q := r.URL.Query()
		names := make([]string, 0, len(q))
		for k := range q {
			names = append(names, k)
		}
		sort.Strings(names)
		log.Printf("api: %s %s key=%s params=[%s] since_minutes=%s limit=%s status=%d bytes=%d dur=%s",
			r.Method, r.URL.Path, key, strings.Join(names, ","),
			orDash(q.Get("since_minutes")), orDash(q.Get("limit")),
			rec.status, rec.bytes, time.Since(start).Round(time.Millisecond))
	})
}

// keySlot carries a pointer from logRequests (outside auth) to noteKey
// (inside it), so the access line can name the key the request used.
type keySlot struct{}

// noteKey records the authenticated key's name for logRequests.
func noteKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if slot, ok := r.Context().Value(keySlot{}).(*string); ok {
			*slot = httpauth.PrincipalFrom(r.Context()).Name
		}
		next(w, r)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}
