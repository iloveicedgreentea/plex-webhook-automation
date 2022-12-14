package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/iloveicedgreentea/go-plex/ezbeq"
	"github.com/iloveicedgreentea/go-plex/models"
	"github.com/spf13/viper"
)

// https://minidsp-rs.pages.dev/cli/master/mute

// based on event type, determine what to do
func minidspRouter(payload models.MinidspRequest, vip *viper.Viper, beqClient *ezbeq.BeqClient) {
	switch {
	case strings.Contains(payload.Command, "off"):
		muteOff(beqClient)
	case strings.Contains(payload.Command, "on"):
		muteOn(beqClient)
	}
}

// send minidsp command via ezbeq
func doMinidspCommand(mute bool, beqClient *ezbeq.BeqClient) {
	r := models.BeqPatchV1{
		Mute: mute,
		MasterVolume: 0,
		Slots: []models.SlotsV1{
			{
				ID: "1",
				Active: true,
				Gains: []float64{0,0},
				Mutes: []bool{mute, mute},
				Entry: "",
			},
		},
	}

	j, err := json.Marshal(r)
	if err != nil {
		log.Error(err)
	}
	log.Debugf("minidsp: sending payload: %s", j)
	beqClient.MakeCommand(j)

}

// TODO: test this
func muteOn(beqClient *ezbeq.BeqClient) {
	log.Debug("Minidsp: running mute on")
	beqClient.MuteCommand(true)
}

func muteOff(beqClient *ezbeq.BeqClient) {
	log.Debug("Minidsp: running mute off")
	beqClient.MuteCommand(false)
}

// process webhook 
func ProcessMinidspWebhook(miniDsp chan<- models.MinidspRequest, vip *viper.Viper) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var payload models.MinidspRequest

		err := json.NewDecoder(r.Body).Decode(&payload)
		if err != nil {
			log.Error(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		
		miniDsp <- payload
	}

	return http.HandlerFunc(fn)
}

// entry point for background tasks
func MiniDspWorker(minidspChan <-chan models.MinidspRequest, vip *viper.Viper) {
	log.Info("Minidsp worker started")

	var beqClient *ezbeq.BeqClient
	var err error

	if vip.GetBool("ezbeq.enabled") {
		log.Debug("Started minidsp worker with ezbeq")
		beqClient, err = ezbeq.NewClient(vip.GetString("ezbeq.url"), vip.GetString("ezbeq.port"))
		if err != nil {
			log.Error(err)
		}
	}

	// block forever until closed so it will wait in background for work
	for i := range minidspChan {
		// determine what to do
		minidspRouter(i, vip, beqClient)
	}
}