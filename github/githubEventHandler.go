package github

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/estafette/estafette-ci-api/config"
	ghcontracts "github.com/estafette/estafette-ci-api/github/contracts"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

// EventHandler handles http events for Github integration
type EventHandler interface {
	Handle(*gin.Context)
	HandlePushEvent(ghcontracts.PushEvent)
	HasValidSignature([]byte, string) (bool, error)
}

type eventHandlerImpl struct {
	eventsChannel                chan ghcontracts.PushEvent
	config                       config.GithubConfig
	prometheusInboundEventTotals *prometheus.CounterVec
}

// NewGithubEventHandler returns a github.EventHandler to handle incoming webhook events
func NewGithubEventHandler(eventsChannel chan ghcontracts.PushEvent, config config.GithubConfig, prometheusInboundEventTotals *prometheus.CounterVec) EventHandler {
	return &eventHandlerImpl{
		eventsChannel:                eventsChannel,
		config:                       config,
		prometheusInboundEventTotals: prometheusInboundEventTotals,
	}
}

func (h *eventHandlerImpl) Handle(c *gin.Context) {

	// https://developer.github.com/webhooks/
	eventType := c.GetHeader("X-Github-Event")
	h.prometheusInboundEventTotals.With(prometheus.Labels{"event": eventType, "source": "github"}).Inc()

	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		log.Error().Err(err).Msg("Reading body from Github webhook failed")
		c.String(http.StatusInternalServerError, "Reading body from Github webhook failed")
		return
	}

	// verify hmac signature
	hasValidSignature, err := h.HasValidSignature(body, c.GetHeader("X-Hub-Signature"))
	if err != nil {
		log.Error().Err(err).Msg("Verifying signature from Github webhook failed")
		c.String(http.StatusInternalServerError, "Verifying signature from Github webhook failed")
		return
	}
	if !hasValidSignature {
		log.Warn().Msg("Signature from Github webhook is invalid")
		c.String(http.StatusBadRequest, "Signature from Github webhook is invalid")
		return
	}

	switch eventType {
	case "push": // Any Git push to a Repository, including editing tags or branches. Commits via API actions that update references are also counted. This is the default event.

		// unmarshal json body
		var pushEvent ghcontracts.PushEvent
		err := json.Unmarshal(body, &pushEvent)
		if err != nil {
			log.Error().Err(err).Str("body", string(body)).Msg("Deserializing body to GithubPushEvent failed")
			return
		}

		h.HandlePushEvent(pushEvent)

	case
		"commit_comment",                        // Any time a Commit is commented on.
		"create",                                // Any time a Branch or Tag is created.
		"delete",                                // Any time a Branch or Tag is deleted.
		"deployment",                            // Any time a Repository has a new deployment created from the API.
		"deployment_status",                     // Any time a deployment for a Repository has a status update from the API.
		"fork",                                  // Any time a Repository is forked.
		"gollum",                                // Any time a Wiki page is updated.
		"installation",                          // Any time a GitHub App is installed or uninstalled.
		"installation_repositories",             // Any time a repository is added or removed from an installation.
		"issue_comment",                         // Any time a comment on an issue is created, edited, or deleted.
		"issues",                                // Any time an Issue is assigned, unassigned, labeled, unlabeled, opened, edited, milestoned, demilestoned, closed, or reopened.
		"label",                                 // Any time a Label is created, edited, or deleted.
		"marketplace_purchase",                  // Any time a user purchases, cancels, or changes their GitHub Marketplace plan.
		"member",                                // Any time a User is added or removed as a collaborator to a Repository, or has their permissions modified.
		"membership",                            // Any time a User is added or removed from a team. Organization hooks only.
		"milestone",                             // Any time a Milestone is created, closed, opened, edited, or deleted.
		"organization",                          // Any time a user is added, removed, or invited to an Organization. Organization hooks only.
		"org_block",                             // Any time an organization blocks or unblocks a user. Organization hooks only.
		"page_build",                            // Any time a Pages site is built or results in a failed build.
		"project_card",                          // Any time a Project Card is created, edited, moved, converted to an issue, or deleted.
		"project_column",                        // Any time a Project Column is created, edited, moved, or deleted.
		"project",                               // Any time a Project is created, edited, closed, reopened, or deleted.
		"public",                                // Any time a Repository changes from private to public.
		"pull_request_review_comment",           // Any time a comment on a pull request's unified diff is created, edited, or deleted (in the Files Changed tab).
		"pull_request_review",                   // Any time a pull request review is submitted, edited, or dismissed.
		"pull_request",                          // Any time a pull request is assigned, unassigned, labeled, unlabeled, opened, edited, closed, reopened, or synchronized (updated due to a new push in the branch that the pull request is tracking). Also any time a pull request review is requested, or a review request is removed.
		"repository",                            // Any time a Repository is created, deleted (organization hooks only), made public, or made private.
		"release",                               // Any time a Release is published in a Repository.
		"status",                                // Any time a Repository has a status update from the API
		"team",                                  // Any time a team is created, deleted, modified, or added to or removed from a repository. Organization hooks only
		"team_add",                              // Any time a team is added or modified on a Repository.
		"watch",                                 // Any time a User stars a Repository.
		"integration_installation_repositories": // ?

	default:
		log.Warn().Str("event", eventType).Msgf("Unsupported Github webhook event of type '%v'", eventType)
	}

	c.String(http.StatusOK, "Aye aye!")
}

func (h *eventHandlerImpl) HandlePushEvent(pushEvent ghcontracts.PushEvent) {
	// test making api calls for github app in the background
	h.eventsChannel <- pushEvent
}

func (h *eventHandlerImpl) HasValidSignature(body []byte, signatureHeader string) (bool, error) {

	// https://developer.github.com/webhooks/securing/
	signature := strings.Replace(signatureHeader, "sha1=", "", 1)
	actualMAC, err := hex.DecodeString(signature)
	if err != nil {
		return false, errors.New("Decoding hexadecimal X-Hub-Signature to byte array failed")
	}

	// calculate expected MAC
	mac := hmac.New(sha1.New, []byte(h.config.WebhookSecret))
	mac.Write(body)
	expectedMAC := mac.Sum(nil)

	// compare actual and expected MAC
	if hmac.Equal(actualMAC, expectedMAC) {
		return true, nil
	}

	log.Warn().
		Str("expectedMAC", hex.EncodeToString(expectedMAC)).
		Str("actualMAC", hex.EncodeToString(actualMAC)).
		Msg("Expected and actual MAC do not match")

	return false, nil
}
