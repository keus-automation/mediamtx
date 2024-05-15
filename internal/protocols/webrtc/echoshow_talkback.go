// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

// ice-restart demonstrates Pion WebRTC's ICE Restart abilities.
package webrtc

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

type udpConn struct {
	conn        *net.UDPConn
	port        int
	payloadType uint8
}

var (
	wsConn             *websocket.Conn
	ffmpegUDPConn      *udpConn
	isConnReadyToWrite bool = false
)

func onAudioTrackHandler(peerConnection *webrtc.PeerConnection, track *webrtc.TrackRemote) {
	go func() {
		ticker := time.NewTicker(time.Second * 3)
		for range ticker.C {
			rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
			if rtcpSendErr != nil {
				fmt.Println(rtcpSendErr)
			}
		}
	}()

	fmt.Printf("Track has started, of type %d %d %d: %s \n",
		track.Codec().Channels, track.Codec().ClockRate,
		track.PayloadType(), track.Codec().RTPCodecCapability.MimeType)

	buf := make([]byte, 5000)
	for {
		i, _, readErr := track.Read(buf)

		// rtpPacket, _, _ := track.ReadRTP()
		// fmt.Println(rtpPacket.PayloadType)
		// rtpPacket.PayloadType = ffmpegUDPConn.payloadType
		// fmt.Println(rtpPacket.PayloadType)

		if readErr != nil {
			if readErr == io.EOF {
				return
			}
			panic(readErr)
		}
		switch track.Kind() {
		case webrtc.RTPCodecTypeAudio:
			if isConnReadyToWrite {
				_, err := ffmpegUDPConn.conn.Write(buf[:i])
				if err != nil {
					fmt.Println("Error", err)
				}
			}
		case webrtc.RTPCodecTypeVideo:
		}
	}
}

func openFFMPEG(sdpPath string) io.ReadCloser {
	ffmpeg := exec.Command(
		"ffmpeg",
		"-protocol_whitelist", "crypto,file,pipe,rtp,udp",
		// "-i", "./input-g72.sdp",
		// "-loglevel", "debug",
		// "-analyzeduration", "20000000",
		"-i", sdpPath,
		// "-i", "./Rev.aac",
		// "-i", "",
		"-vn",
		"-avioflags", "direct",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-acodec", "aac",
		"-profile:a", "aac_low",
		"-fflags", "+flush_packets",
		"-fflags", "discardcorrupt",
		"-flush_packets", "1",
		"-flags", "+global_header",
		"-reset_timestamps", "1",
		"-timeout", "20000000000000",
		"-ar", "22050",
		"-b:a", "16000",
		"-map", "0:a:0",
		"-ac", "1",
		"-muxdelay", "0",
		"-f", "adts",
		"pipe:1",
		// "udp://10.1.2.211:7004?bitrate=24000",
		// "udp://10.1.4.8:7004?bitrate=24000",
		// "udp://192.168.1.68:7004?bitrate=24000",
	)

	// ffmpegIn, _ := ffmpeg.StdinPipe()
	ffmpegOut, _ := ffmpeg.StdoutPipe()
	ffmpegErr, _ := ffmpeg.StderrPipe()
	if err := ffmpeg.Start(); err != nil {
		panic(err)
	}

	go func() {
		scanner := bufio.NewScanner(ffmpegErr)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	}()

	go func() {
		ffmpegBuf := make([]byte, 5000)
		for {
			bufLen, readErr := ffmpegOut.Read(ffmpegBuf)

			if readErr != nil {
				// fmt.Println("FFmpeg read error", readErr)
			} else {
				writeErr := wsConn.WriteMessage(websocket.BinaryMessage, ffmpegBuf[:bufLen])
				if writeErr != nil {
					fmt.Println("Error writing ws message", writeErr)
				}
			}
		}
	}()

	// return ffmpegIn
	return ffmpegOut
}

func openWSConn(wsUrl string) *websocket.Conn {
	fmt.Println("Talkback Url", wsUrl)
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	c, _, err := dialer.Dial(wsUrl, nil)
	if err != nil {
		log.Fatal("dial:", err)
	}

	fmt.Println("Connected to WS", wsUrl)

	isConnReadyToWrite = true

	return c
}

func closeWSConn(conn *websocket.Conn) {
}

func getTalkbackUrl() string {
	requestURL := fmt.Sprintf("http://localhost:%d/talkbackUrl", 3404)
	res, err := http.Get(requestURL)
	if err != nil {
		fmt.Printf("error making http request: %s\n", err)
		os.Exit(1)
	}

	resBody, _ := io.ReadAll(res.Body)

	return string(resBody)
}

func InitializeUDPClient(remotePort int) (*net.UDPConn, error) {
	// Resolve the remote address
	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", remotePort))
	if err != nil {
		return nil, fmt.Errorf("error resolving remote address: %w", err)
	}

	// Resolve the local address with a dynamic port
	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("error resolving local address: %w", err)
	}

	// Dial UDP to establish the connection
	conn, err := net.DialUDP("udp", laddr, raddr)
	if err != nil {
		return nil, fmt.Errorf("error dialing UDP: %w", err)
	}

	// deadline := time.Now().Add(60 * time.Second) // Set timeout to 60 seconds
	// if err := conn.SetReadDeadline(deadline); err != nil {
	// 	fmt.Println("Error setting timeout", err)
	// 	conn.Close() // Attempt to close the connection on error
	// }
	// if err := conn.SetWriteDeadline(deadline); err != nil {
	// 	fmt.Println("Error setting timeout", err)
	// 	conn.Close()
	// }

	return conn, nil
}

func initTalkback() {
	ffmpegUDPConn = &udpConn{
		port:        4000,
		payloadType: 111,
	}
	conn, err := InitializeUDPClient(ffmpegUDPConn.port)
	if err != nil {
		fmt.Println("Error starting UDP client", err)
	}
	ffmpegUDPConn.conn = conn

	talkbackUrl := getTalkbackUrl()
	openFFMPEG("/Users/battuashwik/Desktop/development/code/test/echo-show-test/mediamtx/opus-input.sdp")
	wsConn = openWSConn(talkbackUrl)
}
