package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"
)

// handler is our HTTP handler. It handles muxing, response headers, etc. and
// offloads work to the controller.
type handler struct {
	ctrl *controller
	http *http.Client
}

var _searchTTL = 24 * time.Hour

// newHandler creates a new handler.
func newHandler(ctrl *controller) *handler {
	h := &handler{
		ctrl: ctrl,
		http: &http.Client{},
	}
	return h
}

// newMux registers a handler's routes on a new mux.
func newMux(h *handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/work/{foreignID}", h.getWorkID)
	mux.HandleFunc("/book/{foreignEditionID}", h.getBookID)
	mux.HandleFunc("/book/bulk", h.bulkBook)
	mux.HandleFunc("/author/{foreignAuthorID}", h.getAuthorID)
	mux.HandleFunc("/author/changed", h.getAuthorChanged)

	// Default handler returns 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	return mux
}

// TODO: The client retries on TooManyRequests, but will respect the
// Retry-After (seconds) header. We should account for thundering herds.

// bulkBook is sent as a POST request which isn't cachable. We immediately
// redirect to GET with query params so it can be cached.
//
// The provided IDs are expected to be book (edition) IDs as returned by
// auto_complete.
func (h *handler) bulkBook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var ids []int64

	// If this is a POST, redirect to a GET with query params so the result can
	// be cached.
	if r.Method == http.MethodPost {
		err := json.NewDecoder(r.Body).Decode(&ids)
		if err != nil {
			h.error(w, errors.Join(err, errBadRequest))
			return
		}
		if len(ids) == 0 {
			h.error(w, errMissingIDs)
			return
		}

		query := url.Values{}
		url := url.URL{Path: r.URL.Path}
		for _, id := range ids {
			query.Add("id", fmt.Sprint(id))
		}

		url.RawQuery = query.Encode()

		log(ctx).Debug("redirecting", "url", url.String())
		http.Redirect(w, r, url.String(), http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	// Parse query params.
	for _, idStr := range r.URL.Query()["id"] {
		id, err := pathToID(idStr)
		if err != nil {
			h.error(w, err)
			return
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		h.error(w, errMissingIDs)
		return
	}

	result := bulkBookResource{
		Works:   []workResource{},
		Series:  []seriesResource{},
		Authors: []authorResource{},
	}

	mu := sync.Mutex{}
	wg := sync.WaitGroup{}

	for _, id := range ids {
		wg.Add(1)

		go func(foreignBookID int64) {
			defer wg.Done()

			b, err := h.ctrl.GetBook(ctx, foreignBookID)
			if err != nil {
				if !errors.Is(err, errNotFound) {
					log(ctx).Warn("getting book", "err", err, "bookID", foreignBookID)
				}
				return // Ignore the error.
			}

			var workRsc workResource
			err = json.Unmarshal(b, &workRsc)
			if err != nil {
				return // Ignore the error.
			}

			mu.Lock()
			defer mu.Unlock()

			result.Works = append(result.Works, workRsc)
			result.Series = []seriesResource{}

			// Check if our result already includes this author.
			for _, a := range result.Authors {
				if a.ForeignID == workRsc.Authors[0].ForeignID {
					return // Nothing more to do.
				}
			}

			result.Authors = append(result.Authors, workRsc.Authors...)
		}(id)
	}

	wg.Wait()

	// Collect and de-dupe series -- is this even needed?
	seenSeries := map[int64]bool{}
	for _, a := range result.Authors {
		for _, s := range a.Series {
			if _, seen := seenSeries[s.ForeignID]; seen {
				continue
			}
			seenSeries[s.ForeignID] = true
			result.Series = append(result.Series, s)
		}
	}

	// Sort works by rating count.
	slices.SortFunc(result.Works, func(left, right workResource) int {
		return -cmp.Compare[int64](left.Books[0].RatingCount, right.Books[0].RatingCount)
	})

	cacheFor(w, _searchTTL, true)
	_ = json.NewEncoder(w).Encode(result)
}

// getWorkID handles /work/{id}
//
// Upstream is /work/{workID} which redirects to /book/show/{bestBookID}.
func (h *handler) getWorkID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	workID, err := pathToID(r.URL.Path)
	if err != nil {
		h.error(w, err)
		return
	}

	if r.Method == "DELETE" {
		_ = h.ctrl.cache.Delete(r.Context(), workKey(workID))
		w.WriteHeader(http.StatusOK)
		return
	}

	out, err := h.ctrl.GetWork(ctx, workID)
	if err != nil {
		h.error(w, err)
		return
	}

	cacheFor(w, _workTTL, false)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// cacheFor sets cache response headers. s-maxage controls CDN cache time; we
// default to an hour expiry for clients.
//
// Set varyParams to true if the cache key should include query params.
func cacheFor(w http.ResponseWriter, d time.Duration, varyParams bool) {
	w.Header().Add("Cache-Control", fmt.Sprintf("public, s-maxage=%d, max-age=3600", int(d.Seconds())))
	w.Header().Add("Vary", "Content-Type,Accept-Encoding") // Ignore headers like User-Agent, etc.
	w.Header().Add("Content-Type", "application/json")
	// w.Header().Add("Content-Encoding", "gzip") // TODO: Negotiate this with the client.

	if !varyParams {
		// In most cases we ignore query params when serving cached responses,
		// except for the bulk endpoint and some redirects where these params
		// matter.
		w.Header().Add("No-Vary-Search", "params")
	}
}

// getBookID handles /book/{id}.
//
// Importantly, the client expects this to always return a redirect -- either
// to an author or a work. The work returned is then expected to be "fat" with
// all editions of the work attached to it. This is very large!
//
// (See BookInfoProxy GetEditionInfo.)
//
// Instead, we redirect to `/author/{authorID}?edition={id}` to return the
// necessary structure with only the edition we care about.
func (h *handler) getBookID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	bookID, err := pathToID(r.URL.Path)
	if err != nil {
		h.error(w, err)
		return
	}

	if r.Method == "DELETE" {
		_ = h.ctrl.cache.Delete(r.Context(), bookKey(bookID))
		w.WriteHeader(http.StatusOK)
		return
	}

	b, err := h.ctrl.GetBook(ctx, bookID)
	if err != nil {
		h.error(w, err)
		return
	}

	var workRsc workResource
	err = json.Unmarshal(b, &workRsc)
	if err != nil {
		h.error(w, err)
		return
	}

	cacheFor(w, _editionTTL, false)

	if len(workRsc.Authors) > 0 {
		http.Redirect(w, r, fmt.Sprintf("/author/%d?edition=%d", workRsc.Authors[0].ForeignID, bookID), http.StatusSeeOther)
		return
	}

	// This doesn't actually work -- the client gets a
	// System.NullReferenceException. But we should always have an author, so
	// we should never hit this.
	http.Redirect(w, r, fmt.Sprintf("/work/%d", workRsc.ForeignID), http.StatusSeeOther)
}

// getAuthorID handles /author/{id}.
//
// If an ?edition={bookID} query param is present, as with a /book/{id}
// redirect, an author is returned with only that work/edition.
func (h *handler) getAuthorID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	authorID, err := pathToID(r.URL.Path)
	if err != nil {
		h.error(w, err)
		return
	}

	if r.Method == "DELETE" {
		_ = h.ctrl.cache.Delete(r.Context(), authorKey(authorID))
		w.WriteHeader(http.StatusOK)
		return
	}

	out, err := h.ctrl.GetAuthor(r.Context(), authorID)
	if err != nil {
		h.error(w, err)
		return
	}

	// If a specific edition was requested, mutate the returned author to
	// include only that edition. This satisifies SearchByGRBookId.
	if edition := r.URL.Query().Get("edition"); edition != "" {
		bookID, err := pathToID(edition)
		if err != nil {
			h.error(w, err)
			return
		}
		var author authorResource
		err = json.Unmarshal(out, &author)
		if err != nil {
			h.error(w, err)
			return
		}

		var work workResource
		ww, err := h.ctrl.GetBook(ctx, bookID)
		if err != nil {
			h.error(w, err)
			return
		}

		err = json.Unmarshal(ww, &work)
		if err != nil {
			h.error(w, err)
			return
		}

		author.Works = []workResource{work}

		cacheFor(w, _authorTTL, true)
		_ = json.NewEncoder(w).Encode(author)
		return

	}

	cacheFor(w, _authorTTL, true)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// getAuthorChanged handles the `/author/changed?since={datetime}` endpoint.
//
// Normally this would return IDs for _all_ authors updated since the given
// timestamp -- not just the authors in your library. The query param makes
// this uncachable and it's an expensive operation, so we return nothing and
// force the client to no-op.
//
// As a result, the client will periodically re-query `/author/{id}`:
//   - At least once every 30 days.
//   - Not more than every 12 hours.
//   - At least every 2 days if the author is "continuing" -- which always
//     seems to be the case? I don't think we're respecting end/death times
//     because they aren't returned by us.
//   - Every day if they released a book in the past 30 days, maybe to pick up
//     newer ratings? Unclear.
//
// These will hit cached entries, and the client will pick up newer data
// gradually as entries become invalidated.
func (h *handler) getAuthorChanged(w http.ResponseWriter, _ *http.Request) {
	cacheFor(w, _searchTTL, false)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"Limitted": true, "Ids": []}`))
}

// error writes an error message. The status code defaults to 500 unless the
// error wraps a statusErr.
func (*handler) error(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var s statusErr
	if errors.As(err, &s) {
		status = s.Status()
	}
	http.Error(w, err.Error(), status)
}

var _number = regexp.MustCompile("-?[0-9]+")

func pathToID(p string) (int64, error) {
	p = path.Base(p)
	p = _number.FindString(p)
	i, err := strconv.ParseInt(p, 10, 64)
	if err != nil {
		return 0, errors.Join(err, errBadRequest)
	}
	if i <= 0 {
		return i, errors.Join(fmt.Errorf("expected %d to be positive", i), errBadRequest)
	}
	return i, nil
}
