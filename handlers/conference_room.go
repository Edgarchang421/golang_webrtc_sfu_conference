package handlers

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"webrtc_sfu_conference/conf"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

// websocketwebRTCMessage server、client端之間data解析用途，for Trickle ICE，Event使用Candidate、answer、offer
type websocketwebRTCMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type threadSafeWebSocketWriter struct {
	*websocket.Conn
	sync.Mutex
}

// WriteJSON web Socket 回傳 json
func (t *threadSafeWebSocketWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}

// WriteString web Socket 回傳 String
func (t *threadSafeWebSocketWriter) WriteString(message string) error {
	t.Mutex.Lock()
	defer t.Mutex.Unlock()

	return t.Conn.WriteMessage(websocket.TextMessage, []byte(message))
}

type clientConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWebSocketWriter
}

// ConferenceRoom 帶有所有連線成員、所有成員的track，避免signaling時發生race condition，使用RWMutex
type ConferenceRoom struct {
	// RoomID 單一UUID
	RoomID uuid.UUID

	// conns 連線成員list
	conns []clientConnectionState

	// clientTracks 所有client端track會被加入至此map
	clientTracks map[string]*webrtc.TrackLocalStaticRTP

	// Room建立時間，原使用用途為便於get rooms ID時排序，可棄用
	createdTime time.Time

	// lock 多人讀取，但只有單一寫入
	sync.RWMutex
}

func newConferenceRoom() *ConferenceRoom {
	newRoomID := uuid.New()

	// init map
	newRoomLocalTracks := make(map[string]*webrtc.TrackLocalStaticRTP)

	room := &ConferenceRoom{
		RoomID:       newRoomID,
		clientTracks: newRoomLocalTracks,
		createdTime:  time.Now(),
	}

	allRooms[newRoomID] = room

	// check peerConnection num
	go room.connectionsNumberCheck()
	// Infof("room ID %s created.", newRoomID)
	signalStr := fmt.Sprintf("room ID %s created.", newRoomID.String())

	// new room created signal
	signalingServer.UpdateSignal <- signalStr

	return room
}

// connectionsNumberCheck gc機制?會導致建立過久conference room無法使用
func (r *ConferenceRoom) connectionsNumberCheck() {
	for {
		time.Sleep(10 * time.Minute)
		if len(r.conns) == 0 {
			delete(allRooms, r.RoomID)
			// Infof("delete room ID: %v", r.RoomID)
			signalStr := fmt.Sprintf("room ID %s deleted", r.RoomID.String())

			signalingServer.UpdateSignal <- signalStr
		}
	}
}

// dispatchKeyFrame sends a keyframe to all PeerConnections, used everytime a new user joins the call
func (r *ConferenceRoom) dispatchKeyFrame() {
	// 檢查此room是否還儲存於全局變數rooms中
	if _, ok := allRooms[r.RoomID]; !ok {
		return
	}

	r.Lock()
	defer r.Unlock()

	for i := range r.conns {
		for _, receiver := range r.conns[i].peerConnection.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}

			_ = r.conns[i].peerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}

// Add to list of tracks and fire renegotation for all PeerConnections
func (r *ConferenceRoom) addTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	// 檢查此room是否還儲存於全局變數rooms中
	if _, ok := allRooms[r.RoomID]; !ok {
		return nil
	}

	r.Lock()
	defer func() {
		r.Unlock()
		r.signalPeerConnections()
	}()

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	r.clientTracks[t.ID()] = trackLocal
	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections
func (r *ConferenceRoom) removeTrack(t *webrtc.TrackLocalStaticRTP) {
	// 檢查此room是否還儲存於全局變數rooms中
	if _, ok := allRooms[r.RoomID]; !ok {
		return
	}

	r.Lock()
	defer func() {
		r.Unlock()
		r.signalPeerConnections()
	}()

	delete(r.clientTracks, t.ID())
}

// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks
// 整個signal會for loop所有webrtc peerConnection，目的是檢查是否有沒有同步的track
// 以下為單一peer connection的執行attemptSync()的過程
// 1. 檢查連線狀況
// 2. 檢查peer connection的RTP sender，以及是否有正確放置在會議室中的localTracks map中，使用existingSenders map紀錄，避免重複建立local track發送RTP
// 3. 檢查peer connection的RTP receiver，以及是否有正確放置在會議室中的localTracks map中，使用existingSenders map紀錄，避免建立local track發送給server本身
// 4. for loop檢查trackLocals map，透過existingSenders map比對，existingSenders map中若是沒有trackLocals map的track，則替此peer connection新增此local Track，用於發送影像or音訊
// 5. create & set local offer SDP，然後透過websocket發送至client端。
func (r *ConferenceRoom) signalPeerConnections() {
	// 檢查此room是否還儲存於全局變數rooms中
	if _, ok := allRooms[r.RoomID]; !ok {
		return
	}

	r.Lock()
	defer func() {
		r.Unlock()
		r.dispatchKeyFrame()
	}()

	attemptSync := func() (tryAgain bool) {
		for i := range r.conns {
			if r.conns[i].peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
				r.conns = append(r.conns[:i], r.conns[i+1:]...)
				return true // We modified the slice, start from the beginning
			}

			// map of sender we already are seanding, so we don't double send
			existingSenders := map[string]bool{}

			for _, sender := range r.conns[i].peerConnection.GetSenders() {
				if sender.Track() == nil {
					continue
				}

				existingSenders[sender.Track().ID()] = true

				// If we have a RTPSender that doesn't map to a existing track remove and signal
				if _, ok := r.clientTracks[sender.Track().ID()]; !ok {
					signalingStateCheck(r.conns[i].peerConnection, fmt.Sprintf("pc %v remove track", i))
					if err := r.conns[i].peerConnection.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Don't receive videos we are sending, make sure we don't have loopback
			for _, receiver := range r.conns[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				existingSenders[receiver.Track().ID()] = true
			}

			// Add all track we aren't sending yet to the PeerConnection
			for trackID := range r.clientTracks {
				if _, ok := existingSenders[trackID]; !ok {
					if _, err := r.conns[i].peerConnection.AddTrack(r.clientTracks[trackID]); err != nil {
						return true
					}
				}
			}

			offer, err := r.conns[i].peerConnection.CreateOffer(nil)
			if err != nil {
				return true
			}

			if err = r.conns[i].peerConnection.SetLocalDescription(offer); err != nil {
				return true
			}

			offerString, err := json.Marshal(offer)
			if err != nil {
				return true
			}

			if err = r.conns[i].websocket.WriteJSON(&websocketwebRTCMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}
		}

		return
	}

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			// Release the lock and attempt a sync in 3 seconds. We might be blocking a RemoveTrack or AddTrack
			go func() {
				time.Sleep(time.Second * 3)
				r.signalPeerConnections()
			}()
			return
		}

		if !attemptSync() {
			break
		}
	}
}

// 避免在have local offer的情況下，執行room signal，set local description會失敗
func signalingStateCheck(pc *webrtc.PeerConnection, status string) {
	if pc.SignalingState() != webrtc.SignalingStateStable {
		for {
			// pkg.Debugf("status: %s, non-stable waiting for signaling state stable", status)
			time.Sleep(100 * time.Millisecond)
			if pc.SignalingState() == webrtc.SignalingStateStable || pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
				// pkg.Debugf("Signaling state: %v, pc state: %v, finish signaling state check", pc.SignalingState().String(), pc.ConnectionState().String())
				break
			}
		}
	}
}

func (r *ConferenceRoom) makeRoomInfoResponse() roomInfomation {
	return roomInfomation{
		RoomID:           r.RoomID,
		RoomURL:          fmt.Sprintf("https://%s/room/%s", conf.Domain, r.RoomID.String()),
		RoomWebSocketURL: fmt.Sprintf("wss://%s/room/%s/webSocket", conf.Domain, r.RoomID.String()),
		CreatedTime:      r.createdTime,
	}
}

// roomInfomation use with room create api & get rooms ID webScoket
type roomInfomation struct {
	RoomID           uuid.UUID `json:"roomID"`
	RoomURL          string    `json:"roomURL"`
	RoomWebSocketURL string    `json:"roomWebsocketURL"`
	CreatedTime      time.Time `json:"created_time"`
}
