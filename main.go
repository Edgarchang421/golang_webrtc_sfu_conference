package main

import (
	"log"
	"net/http"
	"time"
	"webrtc_sfu_conference/conf"
	"webrtc_sfu_conference/handlers"

	"github.com/gorilla/mux"
)

func main() {
	r := mux.NewRouter()

	// create room
	r.HandleFunc("/create/room", handlers.CreateRoom).Methods("POST")
	// room websocket webrtc peerConnection endpoint
	r.HandleFunc("/room/{roomid}/webSocket", handlers.JoinMeeting)
	// room page index.html handler
	r.HandleFunc("/room/{roomid}", handlers.RoomPage)

	r.HandleFunc("/index", handlers.IndexPage)
	r.HandleFunc("/roomsinfo/webSocket", handlers.RoomInfoSignaling)
	// conference room id API
	r.HandleFunc("/getRoomsID", handlers.GetRoomIDArray)

	// start HTTP server
	srv := &http.Server{
		Handler: r,
		Addr:    conf.Addr,
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	handlers.Infof("server start")
	log.Fatal(srv.ListenAndServe())
}
