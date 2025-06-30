package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	_ "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

var (
	peerConnection        *webrtc.PeerConnection
	api                   *webrtc.API
	outputTrack           *webrtc.TrackLocalStaticSample
	initOnce              sync.Once
	closeChannel          chan bool
	oggPageDuration       = time.Millisecond * 20
	iceConnectedCtx       context.Context
	iceConnectedCtxCancel context.Context
)

func cleanup() {
	fmt.Println("Cleanup")
	files, err := os.ReadDir("./")
	if err != nil {
		fmt.Println("reading dir failed: %w", err)
		return
	}
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".ogg") || strings.HasSuffix(file.Name(), ".wav") || strings.HasSuffix(file.Name(), ".txt") {
			if err := os.Remove(file.Name()); err != nil {
				fmt.Printf("Failed to delete %s: %v\n", file.Name(), err)
			}
		}
	}
}

type TranscriptionResponse struct {
	Text string `json:"text"`
}

func getSpeechFromText(text string) error {
	apiKey := os.Getenv("OPENAI_API_KEY")

	url := "https://api.openai.com/v1/audio/speech"

	payload := map[string]any{
		"model":        "gpt-4o-mini-tts",
		"input":        text,
		"voice":        "coral",
		"instructions": "Speak in a cheerful and positive tone.",
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		fmt.Println("Error encoding JSON:", err)
		return err
	}

	// Create HTTP POST request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Println("Error creating request:", err)
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	// Send the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error sending request:", err)
		return err
	}
	defer resp.Body.Close()

	// Create output file
	outFile, err := os.Create("final.mp3")
	if err != nil {
		fmt.Println("Error creating output file:", err)
		return err
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, resp.Body)
	if err != nil {
		fmt.Println("Error saving audio file:", err)
		return err
	}

	fmt.Println("Speech saved to final.mp3")

	return nil
}

func writeToTrack(fileName string) {
	fmt.Println("writeToTrack")
	//audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(
	//	webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion",
	//)
	//if audioTrackErr != nil {
	//	panic(audioTrackErr)
	//}

	//rtpSender, audioTrackErr := peerConnection.AddTrack(audioTrack)
	//if audioTrackErr != nil {
	//	panic(audioTrackErr)
	//}
	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	//go func() {
	//	rtcpBuf := make([]byte, 1500)
	//	for {
	//		if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
	//			return
	//		}
	//	}
	//}()

	go func() {
		file, oggErr := os.Open(fileName)
		if oggErr != nil {
			fmt.Println("in os.Open")
			panic(oggErr)
		}

		// Open on oggfile in non-checksum mode.
		ogg, _, oggErr := oggreader.NewWith(file)
		if oggErr != nil {
			fmt.Println("in oggreader")
			panic(oggErr)
		}

		var lastGranule uint64

		<-iceConnectedCtx.Done()
		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(oggPageDuration)
		defer ticker.Stop()
		for ; true; <-ticker.C {
			pageData, pageHeader, oggErr := ogg.ParseNextPage()
			if errors.Is(oggErr, io.EOF) {
				fmt.Printf("All audio pages parsed and sent")
				os.Exit(0)
			}

			if oggErr != nil {
				panic(oggErr)
			}

			// The amount of samples is the difference between the last and current timestamp
			sampleCount := float64(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond
			rtpPacket := media.Sample{Data: pageData, Duration: sampleDuration}
			if oggErr := outputTrack.WriteSample(rtpPacket); oggErr != nil {
				fmt.Println("in write Sample")
				panic(oggErr)
			}
		}
	}()
}

func getTextFromSpeech(fileName string) (string, error) {
	wd, _ := os.Getwd()
	apiKey := os.Getenv("OPENAI_API_KEY")
	audioFilePath := filepath.Join(wd, fileName)

	file, err := os.Open(audioFilePath)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return "", err
	}
	defer file.Close()

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	part, err := writer.CreateFormFile("file", "audio.mp3")
	if err != nil {
		fmt.Println("Error creating form file:", err)
		return "", err
	}
	_, err = io.Copy(part, file)
	if err != nil {
		fmt.Println("Error copying file:", err)
		return "", err
	}

	err = writer.WriteField("model", "gpt-4o-transcribe")
	if err != nil {
		fmt.Println("Error writing form field:", err)
		return "", err
	}

	err = writer.Close()
	if err != nil {
		fmt.Println("Error closing writer:", err)
		return "", err
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/audio/transcriptions", &requestBody)
	if err != nil {
		fmt.Println("Error creating request:", err)
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error sending request:", err)
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Println("Error transcriping:", resp.StatusCode)
		return "", errors.New("Error transcriping")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Error reading response:", err)
		return "", err
	}

	var result TranscriptionResponse

	err = json.Unmarshal(body, &result)
	if err != nil {
		fmt.Println("Error parsing JSON:", err)
		return "", err
	}

	return result.Text, nil
}

func printTranscription(fileName string) {

	txtFileName := fmt.Sprintf("%s.txt", fileName)

	cmd := exec.Command("./bin/whisper-cli", "-l", "auto", "-m", "./models/ggml-large-v3-turbo-q5_0.bin", "-np", "-otxt", "-f", fileName)

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error running whisper %s: %v\n", fileName, err)
		return
	}

	//if err := os.Remove(fileName); err != nil {
	//	fmt.Printf("Failed to delete %s: %v\n", fileName, err)
	//} else {
	//	fmt.Printf("Deleted: %s\n", fileName)
	//}

	readCmd := exec.Command("cat", txtFileName)

	readCmd.Stdout = os.Stdout

	if err := readCmd.Run(); err != nil {
		fmt.Println("error reading txt output")
		fmt.Println(err)
	}

	//if err := os.Remove(txtFileName); err != nil {
	//	fmt.Println("error deleting txt output")
	//	fmt.Println(err)
	//}
}

func convertMp3ToOgg(fileName string) error {
	dir, _ := os.Getwd()

	mp3Path := filepath.Join(dir, fmt.Sprintf("%s.mp3", fileName))
	oggPath := filepath.Join(dir, fmt.Sprintf("%s.ogg", fileName))

	cmd := exec.Command("ffmpeg", "-y", "-i", mp3Path, "-c:a", "libopus", "-page_duration", "20000", oggPath)

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error converting %s: %v\n", oggPath, err)
		return err
	}

	// Delete original OGG after successful conversion
	//if err := os.Remove(oggPath); err != nil {
	//	fmt.Printf("Failed to delete %s: %v\n", oggPath, err)
	//	return err
	//}
	return nil
}

func convertOggToMp3(fileName string) error {
	dir, _ := os.Getwd()

	oggPath := filepath.Join(dir, fmt.Sprintf("%s.ogg", fileName))
	mp3Path := filepath.Join(dir, fmt.Sprintf("%s.mp3", fileName))

	cmd := exec.Command("ffmpeg", "-y", "-i", oggPath, mp3Path)

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error converting %s: %v\n", oggPath, err)
		return err
	}

	// Delete original OGG after successful conversion
	//if err := os.Remove(oggPath); err != nil {
	//	fmt.Printf("Failed to delete %s: %v\n", oggPath, err)
	//	return err
	//}
	return nil
}

func saveToDisk(writer media.Writer, track *webrtc.TrackRemote) {
	defer func() {
		if err := writer.Close(); err != nil {
			panic(err)
		}
	}()
	exist := false
	go func() {
		fmt.Println("listening to close channel")
		<-closeChannel
		fmt.Printf("got close signal")
		exist = true
	}()

	for !exist {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			fmt.Println(err)
			return
		}

		if err := writer.WriteRTP(rtpPacket); err != nil {
			fmt.Println(err)
			return
		}
	}
	fmt.Println("Stopped recording")
}

func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Allow all origins
		w.Header().Set("Access-Control-Allow-Origin", "*")
		// Allow all methods
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		// Allow all headers
		w.Header().Set("Access-Control-Allow-Headers", "*")

		// Handle preflight OPTIONS request
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}

}

func main() {
	closeChannel = make(chan bool)
	defer cleanup()
	http.HandleFunc("/offer", withCORS(offerHandler))
	http.HandleFunc("/stop", withCORS(func(w http.ResponseWriter, r *http.Request) { go stopRecording() }))
	http.HandleFunc("/start", withCORS(func(w http.ResponseWriter, r *http.Request) { go startRecording() }))

	fmt.Println("Server started on :3000")

	err := http.ListenAndServe(":3000", nil)
	if err != nil {
		panic(err)
	}
}

func stopRecording() {
	fmt.Println("Stop")
	closeChannel <- true
	fmt.Println("Added to closeChannel true")
}

func startRecording() {
	fmt.Println("Start")
	closeChannel <- false
	fmt.Println("Added to closeChannel false")
}

func offerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST allowed", http.StatusMethodNotAllowed)
		return
	}

	// Expect raw base64 body
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	var offer webrtc.SessionDescription
	if err := decode(strings.TrimSpace(string(body)), &offer); err != nil {
		http.Error(w, "invalid SDP format", http.StatusBadRequest)
		return
	}

	var pcErr error
	initOnce.Do(func() {
		pcErr = initializePeerConnection()
	})
	if pcErr != nil {
		http.Error(w, "failed to init peer connection", http.StatusInternalServerError)
		return
	}

	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		http.Error(w, fmt.Sprintf("setRemoteDescription failed: %v", err), http.StatusInternalServerError)
		return
	}

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("createAnswer failed: %v", err), http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	if err := peerConnection.SetLocalDescription(answer); err != nil {
		http.Error(w, fmt.Sprintf("setLocalDescription failed: %v", err), http.StatusInternalServerError)
		return
	}

	<-gatherComplete

	encoded := encode(peerConnection.LocalDescription())
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(encoded))
}

func initializePeerConnection() error {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		},
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return err
	}
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		return err
	}
	interceptorRegistry.Add(intervalPliFactory)

	api = webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptorRegistry))

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	peerConnection, err = api.NewPeerConnection(config)
	if err != nil {
		return err
	}

	outputTrack, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion",
	)
	if err != nil {
		return err
	}
	iceConnectedCtx = context.Background()

	rtpSender, err := peerConnection.AddTrack(outputTrack)
	if err != nil {
		return err
	}
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		for {
			fileName := "output"
			oggFile, err := oggwriter.New(fmt.Sprintf("%s.ogg", fileName), 48000, 2)
			if err != nil {
				fmt.Println("Failed to create ogg file:", err)
				continue
			}
			saveToDisk(oggFile, track)
			err = convertOggToMp3(fileName)
			if err != nil {
				fmt.Println("Failed to convert ogg to wav:", err)
				continue
			}
			//printing is to slow
			//printTranscription(fmt.Sprintf("%s.wav", fileName))
			var text string
			text, err = getTextFromSpeech(fmt.Sprintf("%s.mp3", fileName))
			if err != nil {
				fmt.Println("Failed to text from audio:", err)
				continue
			}
			fmt.Println(text)
			err = getSpeechFromText(fmt.Sprintf("this was the users text %s \n my response is very cool, nice one , great, text,text text.", text))
			if err != nil {
				fmt.Println("Failed to get audio from text:", err)
				continue
			}
			err = convertMp3ToOgg("final")
			writeToTrack("final.ogg")
			//"ffmpeg -i input.mp3 -codec:a libvorbis output.ogg"
		}
	})

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		fmt.Printf("Connection state: %s\n", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			fmt.Println("Peer Connection ended, exiting")
			os.Exit(0)
		}
	})

	return nil
}

func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func decode(in string, obj *webrtc.SessionDescription) error {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, obj)
}
