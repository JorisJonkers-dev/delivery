package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// The paths a Canary's three webhooks name, under the gate's endpoint.
const (
	PathMayStart   = "/may-start"
	PathChecks     = "/checks"
	PathMayPromote = "/may-promote"
)

// maxBody bounds a webhook's body: Flagger's payload is a few hundred bytes.
const maxBody = 64 << 10

// payload is what Flagger posts to a webhook (flagger.app/v1beta1, CanaryWebhookPayload). The
// render puts three things in `metadata` and the gate reads nothing else it could not derive.
type payload struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Phase     string            `json:"phase"`
	Metadata  map[string]string `json:"metadata"`
}

var errNotAQuestion = errors.New("gate: the payload does not name a member, its Application and a revision")

func (p payload) question() (Question, error) {
	q := Question{
		Namespace:   p.Namespace,
		Application: p.Metadata["application"],
		Process:     p.Metadata["process"],
		Revision:    p.Metadata["revision"],
	}
	// A Canary is named for the Process it switches; one that says otherwise is not the render's.
	if q.Namespace == "" || q.Application == "" || q.Process == "" || q.Revision == "" || p.Name != q.Process {
		return Question{}, errNotAQuestion
	}
	return q, nil
}

// Routes mounts the three webhooks. Flagger reads a 2xx as yes and anything else as no, so the
// status is the answer: 200 is yes, 409 is "not yet", 400 is a payload that asks nothing, and
// 503 is a question the gate could not answer, which is also no. Flagger records a refusal's
// body on the Canary, so a 409 says what the release waits on.
func (g *Gate) Routes(mux *http.ServeMux, logger *slog.Logger) {
	for path, ask := range map[string]func(context.Context, Question) (Answer, error){
		PathMayStart:   g.MayStart,
		PathChecks:     g.Checks,
		PathMayPromote: g.MayPromote,
	} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			status, why := http.StatusOK, ""
			got, err := asked(r, ask)
			switch {
			case errors.Is(err, errNotAQuestion):
				status, why = http.StatusBadRequest, "the payload names no member, Application and revision"
			case err != nil:
				// The cause may name cluster objects: it is for the log, and the caller learns
				// only that the gate could not answer.
				status, why = http.StatusServiceUnavailable, "the gate could not answer, so the answer is no"
				logger.ErrorContext(r.Context(), "release gate could not answer", "question", path, "error", err)
			case !got.Yes:
				status, why = http.StatusConflict, got.Why
			default:
				why = got.Why
			}
			if status != http.StatusServiceUnavailable {
				logger.InfoContext(r.Context(), "release gate answered", "question", path, "status", status, "why", why)
			}
			// Plain text, never sniffed: the words name Jobs and pods as the cluster holds them.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, why+"\n") //nolint:gosec // G705: text/plain with nosniff is not rendered as markup
		})
	}
}

// asked reads the question a request carries and puts it to ask.
func asked(r *http.Request, ask func(context.Context, Question) (Answer, error)) (Answer, error) {
	var p payload
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody)).Decode(&p); err != nil {
		return Answer{}, errNotAQuestion
	}
	q, err := p.question()
	if err != nil {
		return Answer{}, err
	}
	return ask(r.Context(), q)
}
