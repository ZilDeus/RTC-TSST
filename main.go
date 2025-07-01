package main

import (
	"bytes"
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
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	_ "github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

var (
	peerConnection  *webrtc.PeerConnection
	oggPageDuration = time.Millisecond * 20
	outputTrack     *webrtc.TrackLocalStaticSample
)

/* usefult functions , don't change them */

type TranscriptionResponse struct {
	Text string `json:"text"`
}

func getSpeechFromText(text string, output string) error {
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

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Println("Error creating request:", err)
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error sending request:", err)
		return err
	}
	defer resp.Body.Close()

	outputFileName := fmt.Sprintf("%s.mp3", output)
	outFile, err := os.Create(outputFileName)
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

	fmt.Printf("Speech saved to %s\n", outputFileName)

	return nil
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

func convertMp3ToOgg(fileName string) error {
	dir, _ := os.Getwd()

	mp3Path := filepath.Join(dir, fmt.Sprintf("%s.mp3", fileName))
	oggPath := filepath.Join(dir, fmt.Sprintf("%s.ogg", fileName))

	cmd := exec.Command("ffmpeg", "-y", "-i", mp3Path, "-c:a", "libopus", "-page_duration", "20000", oggPath)

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error converting %s: %v\n", oggPath, err)
		return err
	}

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
		//<-closeChannel
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

func createWebRtcConnection() {
	var err error
	peerConnection, err = webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		return
	}

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		fmt.Printf("Connection state: %s\n", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			fmt.Println("Peer Connection ended, exiting")
			os.Exit(0)
		}
	})

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		//for {
		//	fileName := "output"
		//	oggFile, err := oggwriter.New(fmt.Sprintf("%s.ogg", fileName), 48000, 2)
		//	if err != nil {
		//		fmt.Println("Failed to create ogg file:", err)
		//		continue
		//	}
		//	saveToDisk(oggFile, track)
		//	err = convertOggToMp3(fileName)
		//	if err != nil {
		//		fmt.Println("Failed to convert ogg to wav:", err)
		//		continue
		//	}
		//	var text string
		//	text, err = getTextFromSpeech(fmt.Sprintf("%s.mp3", fileName))
		//	if err != nil {
		//		fmt.Println("Failed to text from audio:", err)
		//		continue
		//	}
		//	fmt.Println(text)
		//	err = getSpeechFromText(fmt.Sprintf("this was the users text %s \n my response is very cool, nice one , great, text,text text.", text))
		//	if err != nil {
		//		fmt.Println("Failed to get audio from text:", err)
		//		continue
		//	}
		//	err = convertMp3ToOgg("final")
		//}
	})

	outputTrack, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion",
	)

	if err != nil {
		panic(err)
	}

	rtpSender, err := peerConnection.AddTrack(outputTrack)
	if err != nil {
		panic(err)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	select {}
}

func main() {

	http.HandleFunc("/offer", withCORS(offerHandler))
	http.HandleFunc("/play", withCORS(func(w http.ResponseWriter, r *http.Request) { go playAudio() }))

	//http.HandleFunc("/stop", withCORS(func(w http.ResponseWriter, r *http.Request) { go stopRecording() }))
	//http.HandleFunc("/start", withCORS(func(w http.ResponseWriter, r *http.Request) { go startRecording() }))

	fmt.Println("starting webrtc server")
	go createWebRtcConnection()

	fmt.Println("Server started on :3000")

	err := http.ListenAndServe(":3000", nil)

	if err != nil {
		panic(err)
	}
}

func stopRecording() {
	fmt.Println("Stop")
	//closeChannel <- true
	//isConnected <- true
	fmt.Println("Added to closeChannel true")
}

func startRecording() {
	fmt.Println("Start")
	//closeChannel <- false
	fmt.Println("Added to closeChannel false")
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

/* usefult functions , don't change them */

/* bad code, clean this shit out please */

func playAudio() {

	go func() {
		// Open a OGG file and start reading using our OGGReader
		file, oggErr := os.Open("final.ogg")
		if oggErr != nil {
			panic(oggErr)
		}

		// Open on oggfile in non-checksum mode.
		ogg, _, oggErr := oggreader.NewWith(file)
		if oggErr != nil {
			panic(oggErr)
		}

		// Keep track of last granule, the difference is the amount of samples in the buffer
		var lastGranule uint64

		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(oggPageDuration)
		defer ticker.Stop()
		for ; true; <-ticker.C {
			pageData, pageHeader, oggErr := ogg.ParseNextPage()
			if errors.Is(oggErr, io.EOF) {
				fmt.Printf("All audio pages parsed and sent")
				return
			}

			if oggErr != nil {
				panic(oggErr)
			}

			// The amount of samples is the difference between the last and current timestamp
			sampleCount := float64(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond
			fmt.Printf("%d %f %d\n", pageHeader.GranulePosition, sampleCount, sampleDuration)

			if oggErr = outputTrack.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); oggErr != nil {
				panic(oggErr)
			}
			fmt.Println("sent audio")
		}
	}()
	select {}
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

/* bad code, clean this shit out please */
