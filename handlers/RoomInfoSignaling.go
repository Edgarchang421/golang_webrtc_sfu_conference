package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type roomsInfoSignalingServer struct {
	// 所有連線，尚未處理斷線client移除自slice
	Clients []*threadSafeWebSocketWriter

	// use rooms info as update signal, data make from getIoomsInfo()
	UpdateSignal chan string

	// lock 多人讀取，但只有單一寫入
	sync.RWMutex
}

func (s *roomsInfoSignalingServer) run() {
	for {
		select {
		case msg := <-s.UpdateSignal:
			Infof(msg)

			roomsInfo := getIoomsInfo()
			infoStr, err := json.Marshal(roomsInfo)
			if err != nil {
				Errorf(err.Error())
				return
			}

			for i, client := range s.Clients {
				if err := client.WriteJSON(
					&websocketwebRTCMessage{
						Event: "info",
						Data:  string(infoStr),
					},
				); err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						Errorf("web socket keep alive Unexpected Close Error: %v", err)
					}

					s.Clients = append(s.Clients[:i], s.Clients[i+1:]...)
				}
			}
		}
	}
}

func newSignalingServer() *roomsInfoSignalingServer {
	s := &roomsInfoSignalingServer{
		Clients:      make([]*threadSafeWebSocketWriter, 0, 10),
		UpdateSignal: make(chan string, 10),
	}

	go s.run()

	return s
}

var signalingServer = newSignalingServer()

func RoomInfoSignaling(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP request to Websocket
	unsafeConn, err := webSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
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

	signalingServer.Lock()
	signalingServer.Clients = append(signalingServer.Clients, wsc)
	signalingServer.Unlock()

	// stop := make(chan struct{})
	keepAliveTicker := time.NewTicker(10 * time.Second)
	message := &websocketwebRTCMessage{}
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
			case "update":
				roomsInfo := getIoomsInfo()

				infoStr, err := json.Marshal(roomsInfo)
				if err != nil {
					Errorf(err.Error())
					return
				}

				if err := wsc.WriteJSON(
					&websocketwebRTCMessage{
						Event: "info",
						Data:  string(infoStr),
					},
				); err != nil {
					keepAliveTicker.Stop()
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						Errorf("web socket keep alive Unexpected Close Error: %v", err)
					}
					return
				}

			}
		}
	}
}

func getIoomsInfo() map[string]interface{} {
	roomsInfo := make(map[string]interface{})

	for k, room := range allRooms {
		state := make([]map[string]interface{}, len(allRooms[k].conns))

		for i, client := range room.conns {
			info := map[string]interface{}{
				"No":                  i,
				"signaling_state":     client.peerConnection.SignalingState().String(),
				"peerConnectio_state": client.peerConnection.ConnectionState().String(),
				"receive_track_num":   len(client.peerConnection.GetReceivers()),
				"send_track_num":      len(client.peerConnection.GetSenders()),
			}

			state[i] = info
		}
		roomsInfo[k.String()] = state
	}

	return roomsInfo
}
