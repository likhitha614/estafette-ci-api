package bitbucket

import (
	"strconv"
	"sync"

	bbcontracts "github.com/estafette/estafette-ci-api/bitbucket/contracts"
	"github.com/estafette/estafette-ci-api/cockroach"
	"github.com/estafette/estafette-ci-api/estafette"
	"github.com/estafette/estafette-ci-contracts"
	manifest "github.com/estafette/estafette-ci-manifest"
	"github.com/rs/zerolog/log"
)

// EventWorker processes events pushed to channels
type EventWorker interface {
	ListenToEventChannels()
	CreateJobForBitbucketPush(bbcontracts.RepositoryPushEvent)
}

type eventWorkerImpl struct {
	waitGroup         *sync.WaitGroup
	stopChannel       <-chan struct{}
	workerPool        chan chan bbcontracts.RepositoryPushEvent
	eventsChannel     chan bbcontracts.RepositoryPushEvent
	apiClient         APIClient
	CiBuilderClient   estafette.CiBuilderClient
	cockroachDBClient cockroach.DBClient
}

// NewBitbucketEventWorker returns the bitbucket.EventWorker
func NewBitbucketEventWorker(stopChannel <-chan struct{}, waitGroup *sync.WaitGroup, workerPool chan chan bbcontracts.RepositoryPushEvent, apiClient APIClient, ciBuilderClient estafette.CiBuilderClient, cockroachDBClient cockroach.DBClient) EventWorker {
	return &eventWorkerImpl{
		waitGroup:         waitGroup,
		stopChannel:       stopChannel,
		workerPool:        workerPool,
		eventsChannel:     make(chan bbcontracts.RepositoryPushEvent),
		apiClient:         apiClient,
		CiBuilderClient:   ciBuilderClient,
		cockroachDBClient: cockroachDBClient,
	}
}

func (w *eventWorkerImpl) ListenToEventChannels() {
	go func() {
		// handle github events via channels
		for {
			// register the current worker into the worker queue.
			w.workerPool <- w.eventsChannel

			select {
			case pushEvent := <-w.eventsChannel:
				go func() {
					w.waitGroup.Add(1)
					w.CreateJobForBitbucketPush(pushEvent)
					w.waitGroup.Done()
				}()
			case <-w.stopChannel:
				log.Debug().Msg("Stopping Bitbucket event worker...")
				return
			}
		}
	}()
}

func (w *eventWorkerImpl) CreateJobForBitbucketPush(pushEvent bbcontracts.RepositoryPushEvent) {

	// check to see that it's a cloneable event
	if len(pushEvent.Push.Changes) == 0 || pushEvent.Push.Changes[0].New == nil || pushEvent.Push.Changes[0].New.Type != "branch" || len(pushEvent.Push.Changes[0].New.Target.Hash) == 0 {
		return
	}

	// get access token
	accessToken, err := w.apiClient.GetAccessToken()
	if err != nil {
		log.Error().Err(err).
			Msg("Retrieving Estafettte manifest failed")
		return
	}

	// get manifest file
	manifestExists, manifestString, err := w.apiClient.GetEstafetteManifest(accessToken, pushEvent)
	if err != nil {
		log.Error().Err(err).
			Msg("Retrieving Estafettte manifest failed")
		return
	}

	if !manifestExists {
		return
	}

	mft, err := manifest.ReadManifest(manifestString)
	builderTrack := "stable"
	hasValidManifest := false
	if err != nil {
		log.Warn().Err(err).Str("manifest", manifestString).Msgf("Deserializing Estafette manifest for repo %v and revision %v failed, continuing though so developer gets useful feedback", pushEvent.Repository.FullName, pushEvent.Push.Changes[0].New.Target.Hash)
	} else {
		builderTrack = mft.Builder.Track
		hasValidManifest = true
	}

	// inject steps
	if hasValidManifest {
		mft, err = estafette.InjectSteps(mft, builderTrack, "bitbucket")
		if err != nil {
			log.Error().Err(err).
				Msg("Failed injecting steps")
			return
		}
	}

	// get authenticated url for the repository
	authenticatedRepositoryURL, err := w.apiClient.GetAuthenticatedRepositoryURL(accessToken, pushEvent.Repository.Links.HTML.Href)
	if err != nil {
		log.Error().Err(err).
			Msg("Retrieving authenticated repository failed")
		return
	}

	// get autoincrement number
	autoincrement, err := w.cockroachDBClient.GetAutoIncrement("bitbucket", pushEvent.Repository.FullName)
	if err != nil {
		log.Error().Err(err).
			Msgf("Failed generating autoincrement for Bitbucket repository %v", pushEvent.Repository.FullName)
	}

	// set build version number
	buildVersion := ""
	buildStatus := "failed"
	if hasValidManifest {
		buildVersion = mft.Version.Version(manifest.EstafetteVersionParams{
			AutoIncrement: autoincrement,
			Branch:        pushEvent.GetRepoBranch(),
			Revision:      pushEvent.GetRepoRevision(),
		})
		buildStatus = "running"
	}

	var labels []contracts.Label
	if hasValidManifest {
		for k, v := range mft.Labels {
			labels = append(labels, contracts.Label{
				Key:   k,
				Value: v,
			})
		}
	}

	var releaseTargets []contracts.ReleaseTarget
	if hasValidManifest {
		for _, r := range mft.Releases {
			releaseTarget := contracts.ReleaseTarget{
				Name:    r.Name,
				Actions: make([]manifest.EstafetteReleaseAction, 0),
			}
			if r.Actions != nil && len(r.Actions) > 0 {
				for _, a := range r.Actions {
					releaseTarget.Actions = append(releaseTarget.Actions, *a)
				}
			}
			releaseTargets = append(releaseTargets, releaseTarget)
		}
	}

	var commits []contracts.GitCommit
	if hasValidManifest {
		for _, c := range pushEvent.Push.Changes {
			if len(c.Commits) > 0 {
				commits = append(commits, contracts.GitCommit{
					Author: contracts.GitAuthor{
						Email:    c.Commits[0].Author.GetEmailAddress(),
						Name:     c.Commits[0].Author.GetName(),
						Username: c.Commits[0].Author.Username,
					},
					Message: c.Commits[0].GetCommitMessage(),
				})
			}
		}
	}

	// store build in db
	insertedBuild, err := w.cockroachDBClient.InsertBuild(contracts.Build{
		RepoSource:     pushEvent.GetRepoSource(),
		RepoOwner:      pushEvent.GetRepoOwner(),
		RepoName:       pushEvent.GetRepoName(),
		RepoBranch:     pushEvent.GetRepoBranch(),
		RepoRevision:   pushEvent.GetRepoRevision(),
		BuildVersion:   buildVersion,
		BuildStatus:    buildStatus,
		Labels:         labels,
		ReleaseTargets: releaseTargets,
		Manifest:       manifestString,
		Commits:        commits,
	})
	if err != nil {
		log.Error().Err(err).
			Msgf("Failed inserting build into db for Bitbucket repository %v", pushEvent.Repository.FullName)
		return
	}

	buildID, err := strconv.Atoi(insertedBuild.ID)
	if err != nil {
		log.Warn().Err(err).Msgf("Failed to convert build id %v to int", insertedBuild.ID)
	}

	// define ci builder params
	ciBuilderParams := estafette.CiBuilderParams{
		JobType:              "build",
		RepoSource:           pushEvent.GetRepoSource(),
		RepoOwner:            pushEvent.GetRepoOwner(),
		RepoName:             pushEvent.GetRepoName(),
		RepoURL:              authenticatedRepositoryURL,
		RepoBranch:           pushEvent.GetRepoBranch(),
		RepoRevision:         pushEvent.GetRepoRevision(),
		EnvironmentVariables: map[string]string{"ESTAFETTE_BITBUCKET_API_TOKEN": accessToken.AccessToken},
		Track:                builderTrack,
		AutoIncrement:        autoincrement,
		VersionNumber:        buildVersion,
		Manifest:             mft,
		BuildID:              buildID,
	}

	// create ci builder job
	if hasValidManifest {
		_, err = w.CiBuilderClient.CreateCiBuilderJob(ciBuilderParams)
		if err != nil {
			log.Error().Err(err).
				Interface("params", ciBuilderParams).
				Msgf("Creating estafette-ci-builder job for Bitbucket repository %v/%v revision %v failed", ciBuilderParams.RepoOwner, ciBuilderParams.RepoName, ciBuilderParams.RepoRevision)

			return
		}
	}
}
