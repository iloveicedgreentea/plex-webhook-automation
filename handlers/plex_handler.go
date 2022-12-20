package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/iloveicedgreentea/go-plex/ezbeq"
	"github.com/iloveicedgreentea/go-plex/mqtt"
	"github.com/iloveicedgreentea/go-plex/homeassistant"
	"github.com/iloveicedgreentea/go-plex/logger"
	"github.com/iloveicedgreentea/go-plex/models"
	"github.com/iloveicedgreentea/go-plex/plex"
	"github.com/spf13/viper"
)

const showItemTitle = "episode"
const movieItemTitle = "movie"

var log = logger.GetLogger()

func decodeWebhook(payload []string) (models.PlexWebhookPayload, error, int) {
	var pwhPayload models.PlexWebhookPayload

	err := json.Unmarshal([]byte(payload[0]), &pwhPayload)
	if err != nil {
		var unmarshalTypeError *json.UnmarshalTypeError

		switch {
		// unmarshall error
		case errors.As(err, &unmarshalTypeError):
			msg := fmt.Sprintf("Request has an invalid value in %q field at position %d", unmarshalTypeError.Field, unmarshalTypeError.Offset)
			return pwhPayload, errors.New(msg), http.StatusBadRequest

		default:
			return pwhPayload, err, http.StatusInternalServerError
		}
	}

	log.Debugf("Received event: %s", pwhPayload.Event)
	return pwhPayload, nil, 0
}

// Sends the payload to the channel for background processing
func ProcessWebhook(plexChan chan<- models.PlexWebhookPayload, vip *viper.Viper) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(0); err != nil {
			log.Error(err)
			return
		}

		payload, hasPayload := r.MultipartForm.Value["payload"]
		if hasPayload {
			decodedPayload, err, statusCode := decodeWebhook(payload)
			if err != nil {
				log.Error(err)
				http.Error(w, err.Error(), statusCode)
			}
			clientUUID := decodedPayload.Player.UUID
			log.Debugf("!!! Your Player UUID is %s !!!!!", clientUUID)

			log.Debugf("Media type is: %s", decodedPayload.Metadata.Type)
			log.Debugf("Media title is: %s", decodedPayload.Metadata.Title)

			// perform function via worker
			if clientUUID != "" {
				if vip.GetString("plex.deviceUUIDFilter") != clientUUID {
					log.Debug("Client UUID does not match filter")
					return
				}
			}
			// only respond to events on a particular account if you share servers and only for movies and shows
			if decodedPayload.Account.Title == vip.GetString("plex.ownerNameFilter") {
				if decodedPayload.Metadata.Type == movieItemTitle || decodedPayload.Metadata.Type == showItemTitle {
					plexChan <- decodedPayload
				}
			}
		}
	}

	return http.HandlerFunc(fn)
}

// does plex send stop if you exit with back button? - Yes, with X for mobile player as well
func mediaStop(vip *viper.Viper, beqClient *ezbeq.BeqClient, haClient *homeassistant.HomeAssistantClient, payload models.PlexWebhookPayload) {

	changeLight(vip, "on")

	if vip.GetBool("ezbeq.enabled") {
		err := beqClient.UnloadBeqProfile(vip.GetBool("ezbeq.dryRun"))
		if err != nil {
			log.Error(err)
			if vip.GetBool("ezbeq.notifyOnLoad") && vip.GetBool("homeAssistant.enabled") {
				err := haClient.SendNotification(fmt.Sprintf("Error UNLOADING profile: %v -- Unsafe to play movies!", err), vip.GetString("ezbeq.notifyEndpointName"))
				if err != nil {
					log.Error()
				}
			}
		}
	}
}
// TODO: add madvr flags
// pause only happens with literally pausing
func mediaPause(vip *viper.Viper, beqClient *ezbeq.BeqClient, haClient *homeassistant.HomeAssistantClient, payload models.PlexWebhookPayload) {
	
	go changeLight(vip, "on")

	if vip.GetBool("ezbeq.enabled") {
		err := beqClient.UnloadBeqProfile(vip.GetBool("ezbeq.dryRun"))
		if err != nil {
			log.Error(err)
			if vip.GetBool("ezbeq.notifyOnLoad") && vip.GetBool("homeAssistant.enabled") {
				err := haClient.SendNotification(fmt.Sprintf("Error UNLOADING profile: %v -- Unsafe to play movies!", err), vip.GetString("ezbeq.notifyEndpointName"))
				if err != nil {
					log.Error()
				}
			}
		}
	}
}

// play is both the "resume" button and play
func mediaPlay(client *plex.PlexClient, vip *viper.Viper, beqClient *ezbeq.BeqClient, haClient *homeassistant.HomeAssistantClient, payload models.PlexWebhookPayload, mediaType string, codec string, edition string) {
	go changeLight(vip, "off")

	go changeAspect(client, payload, vip)
	
	go changeMasterVolume(vip, mediaType)

	if vip.GetBool("ezbeq.enabled") {
		if payload.Metadata.Type == showItemTitle {
			// if its a show and you dont want beq enabled, exit
			if !vip.GetBool("ezbeq.enableTvBeq") {
				return
			}
		}
		log.Debug("Triggering ezBEQ change")
		tmdb := getPlexMovieDb(payload)
		err := beqClient.LoadBeqProfile(tmdb, payload.Metadata.Year, codec, false, "", 0, vip.GetBool("ezbeq.dryRun"), vip.GetString("ezbeq.preferredAuthor"), edition)
		if err != nil {
			log.Error(err)
		}
		// send notification of it loaded
		if vip.GetBool("ezbeq.notifyOnLoad") && vip.GetBool("homeAssistant.enabled") {
			if err != nil {
				err := haClient.SendNotification(fmt.Sprintf("Error loading profile: %v", err), vip.GetString("ezbeq.notifyEndpointName"))
				if err != nil {
					log.Error()
				}
			} else {
				err := haClient.SendNotification(fmt.Sprintf("BEQ Profile: Title - %s  (%d) // Codec %s", payload.Metadata.Title, payload.Metadata.Year, codec), vip.GetString("ezbeq.notifyEndpointName"))
				if err != nil {
					log.Error()
				}
			}
		}
	}
}

// resume is only after pausing as long as the media item is still active
func mediaResume(vip *viper.Viper, beqClient *ezbeq.BeqClient, haClient *homeassistant.HomeAssistantClient, payload models.PlexWebhookPayload, mediaType string, codec string, edition string) {
	// trigger lights
	go changeLight(vip, "off")
	go changeMasterVolume(vip, mediaType)

	// allow skipping search to save time
	if vip.GetBool("ezbeq.enabled") {
		if payload.Metadata.Type == showItemTitle {
			if !vip.GetBool("ezbeq.enableTvBeq") {
				return
			}
		}
		tmdb := getPlexMovieDb(payload)
		// use the cached profile inside the client
		err := beqClient.LoadBeqProfile(tmdb, payload.Metadata.Year, codec, true, beqClient.CurrentProfile, beqClient.MasterVolume, vip.GetBool("ezbeq.dryRun"), vip.GetString("ezbeq.preferredAuthor"), edition)
		if err != nil {
			log.Error(err)
			// no return to continue the rest of actions. Errors dont really matter at this level
		}
		// send notification of it loaded
		if vip.GetBool("ezbeq.notifyOnLoad") && vip.GetBool("homeAssistant.enabled") {
			if err != nil {
				err := haClient.SendNotification(fmt.Sprintf("Error loading profile: %v", err), vip.GetString("ezbeq.notifyEndpointName"))
				if err != nil {
					log.Error()
				}
			} else {
				err := haClient.SendNotification(fmt.Sprintf("BEQ Profile: Title - %s  (%d) // Codec %s", payload.Metadata.Title, payload.Metadata.Year, codec), vip.GetString("ezbeq.notifyEndpointName"))
				if err != nil {
					log.Error()
				}
			}
		}
	}
}

// getEditionName tries to extract the edition from plex or file name. Assumes you have well named files
// Returned types, Unrated, Ultimate, Theatrical, Extended, Director, Criterion
func getEditionName(data models.MediaContainer) string {
	edition := data.Video.EditionTitle
	fileName := strings.ToLower(data.Video.Media.Part.File)

	// if there is an edition from plex metadata, use it
	if edition != "" {
		return edition
	}

	switch {
	case strings.Contains(fileName, "extended"):
		return "Extended"
	case strings.Contains(fileName, "unrated"):
		return "Unrated"
	case strings.Contains(fileName, "theatrical"):
		return "Theatrical"
	case strings.Contains(fileName, "ultimate"):
		return "Ultimate"
	case strings.Contains(fileName, "director"):
		return "Director"
	case strings.Contains(fileName, "criterion"):
		return "Criterion"
	default:
		return ""
	}
}

// based on event type, determine what to do
func eventRouter(client *plex.PlexClient, beqClient *ezbeq.BeqClient, haClient *homeassistant.HomeAssistantClient, payload models.PlexWebhookPayload, vip *viper.Viper) {
	// only trigger on movie or tvshow if that is enabled
	var codec string
	var err error
	var data models.MediaContainer
	var editionName string

	if vip.GetBool("ezbeq.enabled") {
		// make a call to plex to get the data
		data, err = client.GetMediaData(payload.Metadata.Key)
		if err != nil {
			log.Error(err)
		} else {
			// get the edition name
			editionName = getEditionName(data)
			log.Debugf("Found edition: %s", editionName)

			log.Debug("Getting codec from data")
			codec, err = client.GetAudioCodec(data)
			if err != nil {
				log.Errorf("error getting codec: %s", err)

				if codec == "" {
					log.Error("ezBEQ is enabled but codec is blank. Not loading BEQ for safety")
				}

				return
			}
		}
	}

	log.Debugf("Received codec: %s", codec)
	log.Debugf("Got media type of: %s ", payload.Metadata.Type)

	switch payload.Event {
	// unload BEQ on pause OR stop because I never press stop, just pause and then back.
	case "media.stop":
		log.Debug("media.stop received")
		mediaStop(vip, beqClient, haClient, payload)
	case "media.pause":
		log.Debug("media.pause received")
		mediaPause(vip, beqClient, haClient, payload)
	// play means a new file was started
	case "media.play":
		log.Debug("media.play received")
		mediaPlay(client, vip, beqClient, haClient, payload, payload.Metadata.Type, codec, editionName)
	// Pressing the 'resume' button actually is media.play thankfully
	case "media.resume":
		log.Debug("media.resume received")
		mediaResume(vip, beqClient, haClient, payload, payload.Metadata.Type, codec, editionName)
	case "media.scrobble":
	default:
		log.Infof("Received unsupported event: %s", payload.Event)
	}
}

// get the imdb ID from plex metadata
func getPlexImdbID(payload models.PlexWebhookPayload) string {
	// try to get IMDB title from plex to save time
	for _, model := range payload.Metadata.GUID0 {
		if strings.Contains(model.ID, "imdb") {
			log.Debug("Got title ID from plex")
			return strings.Split(model.ID, "imdb://")[1]
		}
	}

	return ""
}

// get the tmdb ID from plex metadata
func getPlexMovieDb(payload models.PlexWebhookPayload) string {
	// try to get IMDB title from plex to save time
	for _, model := range payload.Metadata.GUID0 {
		if strings.Contains(model.ID, "tmdb") {
			log.Debug("Got tmdb ID from plex")
			return strings.Split(model.ID, "tmdb://")[1]
		}
	}

	return ""
}

// will change aspect ratio
func changeAspect(client *plex.PlexClient, payload models.PlexWebhookPayload, vip *viper.Viper) {
	if vip.GetBool("homeAssistant.triggerAspectRatioChangeOnEvent") && vip.GetBool("homeAssistant.enabled") {
		log.Debug("Changing aspect ratio")
		// lookup aspect based on imdb technical info
		imdbID := getPlexImdbID(payload)
		aspect, err := client.GetAspectRatio(payload.Metadata.Title, payload.Metadata.Year, imdbID)
		if err != nil {
			log.Error(err)
			return
		}
		// handle logic for aspect ratios
		topic := vip.GetString("mqtt.topicAspectratio")
		switch {
		// 2.3 -> 2.4+ scope
		case aspect >= 2.3:
			err := mqtt.Publish(vip, []byte("{\"aspect\":\"2.4\"}"), topic)
			if err != nil {
				log.Error()
			}
		// 2.1 -> 2.29
		case aspect >= 2.1 && aspect < 2.3:
			err := mqtt.Publish(vip, []byte("{\"aspect\":\"2.2\"}"), topic)
			if err != nil {
				log.Error()
			}
		// for 1.85 -> 2.1 basically intention is to zoom aspect ratio, not adjust lens IMO
		case aspect > 1.78 && aspect < 2.1:
			err := mqtt.Publish(vip, []byte("{\"aspect\":\"1.85\"}"), topic)
			if err != nil {
				log.Error()
			}
		// for 1.78 -> 16:9
		default:
			err := mqtt.Publish(vip, []byte("{\"aspect\":\"1.78\"}"), topic)
			if err != nil {
				log.Error()
			}
		}
	}
}

// trigger HA for MV change per type
func changeMasterVolume(vip *viper.Viper, mediaType string) {
	if vip.GetBool("homeAssistant.triggerAvrMasterVolumeChangeOnEvent") && vip.GetBool("homeAssistant.enabled") {
		err := mqtt.Publish(vip, []byte(fmt.Sprintf("{\"type\":\"%s\"}", mediaType)), vip.GetString("mqtt.topicVolume"))
		if err != nil {
			log.Error()
		}
	}
}

// trigger HA for light change given entity and desired state
func changeLight(vip *viper.Viper, state string) {
	if vip.GetBool("homeAssistant.triggerLightsOnEvent") && vip.GetBool("homeAssistant.enabled") {
		log.Debug("Changing light")
		err := mqtt.Publish(vip, []byte(fmt.Sprintf("{\"state\":\"%s\"}", state)), vip.GetString("mqtt.topicLights"))
		if err != nil {
			log.Error()
		}
	}
}

// entry point for background tasks
func PlexWorker(plexChan <-chan models.PlexWebhookPayload, vip *viper.Viper) {
	log.Info("PlexWorker started")

	var beqClient *ezbeq.BeqClient
	var haClient *homeassistant.HomeAssistantClient
	var err error

	// Server Info
	plexClient := plex.NewClient(vip.GetString("plex.url"), vip.GetString("plex.port"))

	if vip.GetBool("ezbeq.enabled") {
		log.Info("Started with ezbeq enabled")
		beqClient, err = ezbeq.NewClient(vip.GetString("ezbeq.url"), vip.GetString("ezbeq.port"))
		if err != nil {
			log.Error(err)
		}
		// unload existing profile for safety
		err = beqClient.UnloadBeqProfile(false)
		if err != nil {
			log.Errorf("Error on startup - unloading beq %v", err)
		}
	}
	if vip.GetBool("homeAssistant.enabled") {
		log.Info("Started with HA enabled")
		haClient = homeassistant.NewClient(vip.GetString("homeAssistant.url"), vip.GetString("homeAssistant.port"), vip.GetString("homeAssistant.token"))
	}
	// block forever until closed so it will wait in background for work
	for i := range plexChan {
		// determine what to do
		eventRouter(plexClient, beqClient, haClient, i, vip)
	}
}
