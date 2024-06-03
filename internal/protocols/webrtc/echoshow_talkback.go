package webrtc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

type TalkbackRequest struct {
	CameraId string `json:"cameraId"`
}
type TalkbackResponse struct {
	Success bool   `json:"success"`
	Data    string `json:"data"`
	Error   string `json:"error"`
}

type TalkbackManager struct {
	cameraId         string
	ffmpegInputReady chan bool
	udpConn          *net.UDPConn
	udpPort          int
	wsConn           *websocket.Conn
	ffmpegCmd        *exec.Cmd
	ctx              context.Context
	cancel           context.CancelFunc
}

func InitTalkback(cameraId string) (*TalkbackManager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	talkbackInst := &TalkbackManager{
		cameraId:         cameraId,
		ffmpegInputReady: make(chan bool),
		udpConn:          nil,
		udpPort:          generateRandomPort(),
		ctx:              ctx,
		cancel:           cancel,
	}

	talkbackUrl, err := getTalkbackUrl(cameraId)
	if err != nil {
		fmt.Println("Error getting talkback Url:", err)
		closeConnections(talkbackInst)
		return nil, fmt.Errorf("talkback error")
	}
	if !talkbackUrl.Success {
		fmt.Printf("Error in talkback response: %s\n", talkbackUrl.Error)
		closeConnections(talkbackInst)
		return nil, fmt.Errorf("talkback error")
	}

	wsConn, wsErr := openWSConn(talkbackUrl.Data)
	if wsErr != nil {
		fmt.Printf("Error in talkback ws connection: %s\n", wsErr)
		closeConnections(talkbackInst)
		return nil, fmt.Errorf("talkback error")
	}
	talkbackInst.wsConn = wsConn

	udpConn, err := openUDPClient(talkbackInst.udpPort)
	if err != nil {
		fmt.Println("Error starting UDP client:", err)
		closeConnections(talkbackInst)
	}
	talkbackInst.udpConn = udpConn

	if err := startFFMPEG(talkbackInst); err != nil {
		fmt.Println("Error monitoring FFmpeg:", err)
		closeConnections(talkbackInst)
		return nil, fmt.Errorf("talkback error")
	}

	return talkbackInst, nil
}

func generateRandomPort() int {
	// Define the port range
	min := 3000
	max := 8000

	// Generate a random integer between min and max
	randomPort := rand.Intn(max-min+1) + min
	return randomPort
}

func onAudioTrackHandler(peerConnection *webrtc.PeerConnection, track *webrtc.TrackRemote, talkbackInst *TalkbackManager) {
	go func() {
		ticker := time.NewTicker(time.Second * 3)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
				if rtcpSendErr != nil {
					fmt.Println(rtcpSendErr)
				}
			case <-talkbackInst.ctx.Done():
				return
			}
		}
	}()

	fmt.Printf("Track has started, of type %d %d %d: %s \n",
		track.Codec().Channels, track.Codec().ClockRate,
		track.PayloadType(), track.Codec().RTPCodecCapability.MimeType)

	shouldWriteToFFMPEG := false

	go func() {
		for {
			select {
			case val := <-talkbackInst.ffmpegInputReady:
				fmt.Println("this is should write to ffmpeg", val)
				shouldWriteToFFMPEG = val
			case <-talkbackInst.ctx.Done():
				return
			}
		}
	}()

	buf := make([]byte, 5000)
	for {
		select {
		case <-talkbackInst.ctx.Done():
			return
		default:
			i, _, readErr := track.Read(buf)
			if readErr != nil {
				if readErr == io.EOF {
					return
				}
				fmt.Printf("Error reading track: %v\n", readErr)
				return
			}

			if track.Kind() == webrtc.RTPCodecTypeAudio && shouldWriteToFFMPEG {
				_, err := talkbackInst.udpConn.Write(buf[:i])
				if err != nil {
					fmt.Println("Error writing to UDP:", err)
				}
				talkbackInst.udpConn.SetWriteDeadline(time.Now().Add(20 * time.Second))
			}
		}
	}
}

func startFFMPEG(talkbackInst *TalkbackManager) error {
	fmt.Println("Starting ffmpeg", talkbackInst.cameraId)

	ffmpeg := exec.Command(
		"ffmpeg",
		"-protocol_whitelist", "crypto,file,pipe,rtp,udp",
		"-f", "sdp",
		"-i", "pipe:0",
		"-vn",
		"-acodec", "aac_at",
		"-flush_packets", "1",
		"-flags", "+global_header",
		"-reset_timestamps", "1",
		"-ar", "22050",
		"-b:a", "16000",
		"-q:a", "2",
		"-ac", "1",
		"-map", "0:a:0",
		"-muxdelay", "0",
		"-f", "adts",
		"pipe:1",
	)

	ffmpegIn, err := ffmpeg.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create ffmpeg stdin pipe: %w", err)
	}
	ffmpegOut, err := ffmpeg.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create ffmpeg stdout pipe: %w", err)
	}
	ffmpegErr, err := ffmpeg.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create ffmpeg stderr pipe: %w", err)
	}
	if err := ffmpeg.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	talkbackInst.ffmpegCmd = ffmpeg
	ffmpegIn.Write([]byte(generateSdp(talkbackInst.udpPort)))
	ffmpegIn.Close()

	go func() {
		scanner := bufio.NewScanner(ffmpegErr)
		for scanner.Scan() {
			text := scanner.Text()
			fmt.Println(text)
			if strings.Contains(text, "KeusEchoShow") {
				fmt.Println("started ffmpeg output")
				talkbackInst.ffmpegInputReady <- true
				return
			}
		}
	}()

	go func() {
		ffmpegBuf := make([]byte, 2500)
		for {
			select {
			case <-talkbackInst.ctx.Done():
				return
			default:
				bufLen, readErr := ffmpegOut.Read(ffmpegBuf)
				if readErr != nil {
					if readErr == io.EOF {
						return
					}
					fmt.Println("FFmpeg read error", readErr)
					continue
				}
				writeErr := talkbackInst.wsConn.WriteMessage(websocket.BinaryMessage, ffmpegBuf[:bufLen])
				if writeErr != nil {
					fmt.Println("Error writing ws message", writeErr)
				}
			}
		}
	}()

	return nil
}

func generateSdp(port int) string {
	sdp := []string{"v=0",
		"o=- 0 0 IN IP4 127.0.0.1",
		"s=KeusEchoShow",
		"c=IN IP4 127.0.0.1",
		"t=0 0",
		"m=audio " + strconv.Itoa(port) + " RTP/AVP 96",
		"a=rtpmap:96 OPUS/48000/2"}

	temp := strings.Join(sdp, "\n")
	fmt.Println("final sdp -", temp, port, strconv.Itoa(port))
	return temp
}

func openWSConn(wsUrl string) (*websocket.Conn, error) {
	fmt.Println("Talkback Url", wsUrl)
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	c, _, err := dialer.Dial(wsUrl, nil)

	if err != nil {
		return nil, fmt.Errorf("error connecting to WebSocket: %w", err)
	}

	fmt.Println("Connected to WS", wsUrl)

	return c, nil
}

func getTalkbackUrl(cameraId string) (TalkbackResponse, error) {
	url := "http://localhost:4444/getTalkbackUrl"

	reqBody := TalkbackRequest{CameraId: cameraId}
	reqBodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return TalkbackResponse{}, fmt.Errorf("failed to marshal request body: %v", err)
	}

	fmt.Println("Request body:", reqBody)

	req, err := http.NewRequest("GET", url, bytes.NewBuffer(reqBodyBytes))
	if err != nil {
		return TalkbackResponse{}, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)

	fmt.Println("response body:", resp)

	if err != nil {
		return TalkbackResponse{}, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	var response TalkbackResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return TalkbackResponse{}, fmt.Errorf("failed to decode response: %v", err)
	}

	return response, nil
}

func openUDPClient(port int) (*net.UDPConn, error) {
	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("error resolving remote address: %w", err)
	}

	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("error resolving local address: %w", err)
	}

	conn, err := net.DialUDP("udp", laddr, raddr)
	if err != nil {
		return nil, fmt.Errorf("error dialing UDP: %w", err)
	}

	timeout := 20 * time.Second
	conn.SetReadDeadline(time.Now().Add(timeout))
	conn.SetWriteDeadline(time.Now().Add(timeout))

	return conn, nil
}

func closeConnections(talkbackInst *TalkbackManager) {
	if talkbackInst == nil {
		return
	}

	if talkbackInst.cancel != nil {
		talkbackInst.cancel()
	}

	if talkbackInst.wsConn != nil {
		talkbackInst.wsConn.Close()
	}

	if talkbackInst.udpConn != nil {
		talkbackInst.udpConn.Close()
	}

	if talkbackInst.ffmpegCmd != nil {
		talkbackInst.ffmpegCmd.Process.Kill()
	}
}
