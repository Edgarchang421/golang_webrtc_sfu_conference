package handlers

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sort"
	"sync"
	"text/template"
	"time"
	"webrtc_sfu_conference/conf"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

var allRooms = newWebRTCServer()

type webRTCServer struct {
	Rooms map[uuid.UUID]*ConferenceRoom
	sync.RWMutex
}

func newWebRTCServer() *webRTCServer {
	return &webRTCServer{
		Rooms: make(map[uuid.UUID]*ConferenceRoom),
	}
}

// webSocketUpgrader 使用於webRTC peerConnection建立，需要做 CORS Domain 給外部的服務作為連接使用，因此always return true
// 此func主要作為避免跨站點攻擊 cross-site request forgery。
var webSocketUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func CreateRoom(w http.ResponseWriter, r *http.Request) {
	room := newConferenceRoom()

	roomInfo := room.makeRoomInfoResponse()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(roomInfo); err != nil {
		Errorf("json encode err: %v", err)
		return
	}
}

func RoomPage(w http.ResponseWriter, r *http.Request) {
	var oldIndexTemplate = &template.Template{}

	indexHTML, err := ioutil.ReadFile("./static/room.html")
	if err != nil {
		panic(err)
	}
	oldIndexTemplate = template.Must(template.New("").Parse(string(indexHTML)))

	// get url room id parameters
	vars := mux.Vars(r)
	inputRoomUUID := vars["roomid"]

	roomID, err := uuid.Parse(inputRoomUUID)
	if err != nil {
		Errorf("url room UUID error: %v", err)
		return
	}

	if _, ok := allRooms.Rooms[roomID]; !ok {
		// room doesn't exist]
		Errorf("roomID %v not exist error", roomID)
		return
	}

	webSocketURL := fmt.Sprintf("wss://%s/room/%s/webSocket", conf.Domain, roomID.String())

	if err := oldIndexTemplate.Execute(w, webSocketURL); err != nil {
		log.Fatal(err)
	}
}

func IndexPage(w http.ResponseWriter, r *http.Request) {
	var oldIndexTemplate = &template.Template{}

	html, err := ioutil.ReadFile("./static/index.html")
	if err != nil {
		Errorf(err.Error())
		return
	}
	oldIndexTemplate = template.Must(template.New("").Parse(string(html)))

	webSocketURL := fmt.Sprintf("wss://%s/roomsinfo/webSocket", conf.Domain)

	if err := oldIndexTemplate.Execute(w, webSocketURL); err != nil {
		log.Fatal(err)
	}
}

// Client 端加入單一房間，web socket endpoint
func JoinMeeting(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP request to Websocket
	unsafeConn, err := webSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		Errorf("upgrade err: %v", err)
		http.Error(w, fmt.Sprintf("webSocket Error: %v", err), http.StatusInternalServerError)
		return
	}

	wsc := &threadSafeWebSocketWriter{
		unsafeConn,
		sync.Mutex{},
	}

	// When this frame returns close the Websocket
	defer func() {
		if cErr := wsc.Close(); cErr != nil {
			if websocket.IsUnexpectedCloseError(cErr, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				Errorf("web socket read message Unexpected Close Error: %v", cErr)
				return
			}
		}
	}()

	// Create new PeerConnection
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		Errorf("peerConnection create err: %v", err)
		http.Error(w, fmt.Sprintf("webRTC PeerConnection Create Error: %v", err), http.StatusInternalServerError)
		return
	}

	// When this frame returns close the PeerConnection
	defer func() {
		if cErr := pc.Close(); cErr != nil {
			Errorf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	// Accept one audio and two video track incoming
	// 若有兩個video需求，此處需要正確設定RTP Codec Type以及數量
	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		// AddTransceiverFromKind最終會使用addRTPTransceiver func，addRTPTransceiver會觸發On Negotiation needed Event
		if _, err := pc.AddTransceiverFromKind(typ,
			webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionRecvonly,
			},
		); err != nil {
			Errorf("peerConnection AddTransceiverFromKind err: %v", err)
			http.Error(w, fmt.Sprintf("webRTC PeerConnection AddTransceiverFromKind err: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// get url room id，唯一與原程式碼不同處，判斷URL的roomid，選擇會議室
	vars := mux.Vars(r)
	inputRoomUUID := vars["roomid"]

	roomID, err := uuid.Parse(inputRoomUUID)
	if err != nil {
		Errorf("url room UUID error: %v", err)
		http.Error(w, fmt.Sprintf("URL room UUID Format error: %v", err), http.StatusBadRequest)
		return
	}

	if _, ok := allRooms.Rooms[roomID]; !ok {
		Errorf("room %v not exist", roomID)
		http.Error(w, fmt.Sprintf("room %v doesn't exist", roomID), http.StatusBadRequest)
		return
	}

	room := allRooms.Rooms[roomID]

	room.Lock()
	room.conns = append(room.conns, clientConnectionState{pc, wsc})
	room.Unlock()

	// pcIndex := len(room.conns) - 1

	// Trickle ICE. Emit server candidate to client
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		candidateString, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			Errorf("candidateString json Marshal error: %v", err)
			return
		}
		// pkg.Debugf("Peer Connection %v on candidate", pcIndex)

		if err = wsc.WriteJSON(&websocketwebRTCMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); err != nil {
			Errorf("webScoket write Json error: %v", err)
		}
	})

	// If PeerConnection is closed remove it from global list
	pc.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := pc.Close(); err != nil {
				Errorf("PeerConnection Close error: %v", err)
			}
		case webrtc.PeerConnectionStateClosed:
			// signalingStateCheck(pc, fmt.Sprintf("Peer Connection %v closed", pcIndex))
			room.signalPeerConnections()
		}
	})

	// webRTC PeerConnection接收remote Track的event handler，新增track至local tracks map
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		Infof("--------------------Peer Connection OnTrack Remote track ID : %v--------------------", t.ID())
		// Debugf("Peer Connection ontrack signaling state: %v", pc.SignalingState())

		signalingStateCheck(pc, "pc ontrack")
		// Create a track to fan out our incoming video to all peers
		trackLocal := room.addTrack(t)
		defer func() {
			// signalingStateCheck(pc, fmt.Sprintf("Peer Connection %v ontrack over, have to remove track", pcIndex))
			room.removeTrack(trackLocal)
		}()

		buf := make([]byte, 1500)
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			if _, err = trackLocal.Write(buf[:i]); err != nil {
				return
			}
		}
	})

	// pc.OnSignalingStateChange(func(s webrtc.SignalingState) {
	// 	pkg.Debugf("Peer Connection %v signaling state change: %v", pcIndex, s)
	// })

	// pc.OnNegotiationNeeded(func() {
	// 	pkg.Debugf("On Negotiation needed")
	// })

	// Signal for the new PeerConnection
	room.signalPeerConnections()

	webSocketData := make(chan []byte)
	stop := make(chan struct{}) // stop signal
	go func() {
		for {
			_, rawData, err := wsc.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					Errorf("web socket read message unexpected close error: %v", err)
				}
				close(stop)
				break
			}
			webSocketData <- rawData
		}
	}()

	message := &websocketwebRTCMessage{}
	keepAliveTicker := time.NewTicker(10 * time.Second)

	for {
		select {
		case <-keepAliveTicker.C:
			if err := wsc.WriteJSON(
				&websocketwebRTCMessage{
					Event: "keepalive",
				},
			); err != nil {
				keepAliveTicker.Stop()
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					Errorf("web socket keep alive Unexpected Close Error: %v", err)
				}
				return
			}

		case <-stop:
			return

		case data := <-webSocketData:
			err := json.Unmarshal(data, &message)
			if err != nil {
				Errorf("web socket message json parse error: %v", err)
				return
			}

			switch message.Event {
			case "candidate":
				candidate := webrtc.ICECandidateInit{}
				if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
					Errorf("webSocket candidate message json.Unmarshal error: %v", err)
					return
				}

				if err := pc.AddICECandidate(candidate); err != nil {
					Errorf("peerConnection AddICECandidate error: %v", err)
					return
				}
			case "answer":
				answer := webrtc.SessionDescription{}
				if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
					Errorf("webSocket answer message json.Unmarshal error: %v", err)
					return
				}

				if err := pc.SetRemoteDescription(answer); err != nil {
					Errorf("peerConnection SetRemoteDescription error: %v", err)
					return
				}
			case "addTrack":
				offer, err := pc.CreateOffer(nil)
				if err != nil {
					Errorf("add multipal track create offer error: %v", err)
					return
				}
				err = pc.SetLocalDescription(offer)
				if err != nil {
					Errorf("add multipal track Set Local Description error: %v", err)
					return
				}
				offerString, err := json.Marshal(offer)
				if err != nil {
					Errorf("add multipal track offer json.Marshal error: %v", err)
					return
				}

				if err = wsc.WriteJSON(&websocketwebRTCMessage{
					Event: "offer",
					Data:  string(offerString),
				}); err != nil {
					Errorf("add multipal track offer write json error: %v", err)
					return
				}
			}
		}
	}
}

// GetRoomIDArray 單次獲取現在room info的API
func GetRoomIDArray(w http.ResponseWriter, r *http.Request) {
	roomsInfo := make([]roomInfomation, 0, len(allRooms.Rooms))
	for i := range allRooms.Rooms {
		roomsInfo = append(roomsInfo, allRooms.Rooms[i].makeRoomInfoResponse())
	}

	// sorted by time
	sort.Slice(roomsInfo,
		func(i, j int) bool {
			return roomsInfo[i].CreatedTime.Before(roomsInfo[j].CreatedTime)
		},
	)

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(roomsInfo)
	if err != nil {
		Errorf("getRoomIDArray json encode err: %v", err)
		return
	}
}

// dispatchKeyFrame sends a keyframe to all PeerConnections, used everytime a new user joins the call
func DispatchKeyFrameToAll() {
	for _, room := range allRooms.Rooms {
		room.Lock()
		defer room.Unlock()

		for i := range room.conns {
			for _, receiver := range room.conns[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				_ = room.conns[i].peerConnection.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{
						MediaSSRC: uint32(receiver.Track().SSRC()),
					},
				})
			}
		}
	}
}
