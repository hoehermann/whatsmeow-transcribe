// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// This is a trimmed copy of https://github.com/tulir/whatsmeow/blob/main/mdtest/main.go
// with the getTranscription function added.

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var cli *whatsmeow.Client
var log waLog.Logger

var quitter = make(chan struct{})

var logLevel = "INFO"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var dbAddress = flag.String("db-address", "file:whatsmeow.db?_foreign_keys=on", "Database address")
var apiUrl = flag.String("api-url", "https://api.openai.com/v1/audio/transcriptions", "Transcription API URL")
var apiKey = flag.String("api-key", "", "Transcription API Key")
var messageHead = flag.String("message-head", "Transcript:\n> ", "Text to start message with")

func main() {
	waBinary.IndentXML = true
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}
	if *apiKey == "" {
		*apiKey = os.Getenv("API_KEY")
	}
	store.DeviceProps.RequireFullSync = proto.Bool(false)
	store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{
		FullSyncDaysLimit:   proto.Uint32(0),
		FullSyncSizeMbLimit: proto.Uint32(0),
		StorageQuotaMb:      proto.Uint32(0),
	}
	log = waLog.Stdout("Main", logLevel, true)

	store.DeviceProps.Os = proto.String("whatsmeow-transcribe")

	dbLog := waLog.Stdout("Database", logLevel, true)
	storeContainer, err := sqlstore.New(*dbDialect, *dbAddress, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to database: %v", err)
		return
	}
	device, err := storeContainer.GetFirstDevice()
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	cli = whatsmeow.NewClient(device, waLog.Stdout("Client", logLevel, true))
	cli.PrePairCallback = func(jid types.JID, platform, businessName string) bool {
		log.Infof("Pairing %s (platform: %q, business name: %q).", jid, platform, businessName)
		return true
	}

	ch, err := cli.GetQRChannel(context.Background())
	if err != nil {
		// This error means that we're already logged in, so ignore it.
		if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			log.Errorf("Failed to get QR channel: %v", err)
		}
	} else {
		go func() {
			for evt := range ch {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				} else {
					log.Infof("QR channel result: %s", evt.Event)
				}
			}
		}()
	}

	cli.AddEventHandler(handler)
	err = cli.Connect()
	if err != nil {
		log.Errorf("Failed to connect: %v", err)
		return
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	for {
		select {
		case <-c:
			log.Infof("Interrupt received, exiting")
			cli.Disconnect()
			return
		case <-quitter:
			log.Infof("Shutdown requested, exiting")
			return
		}
	}
}

func handler(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.StreamReplaced, *events.Disconnected:
		log.Infof("Got %+v. Terminating.", evt)
		close(quitter)
	case *events.Message:
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		if evt.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", evt.Info.Type))
		}
		if evt.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", evt.Info.Category))
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "ephemeral")
		}
		if evt.IsViewOnceV2 {
			metaParts = append(metaParts, "ephemeral (v2)")
		}
		if evt.IsDocumentWithCaption {
			metaParts = append(metaParts, "document with caption")
		}
		if evt.IsEdit {
			metaParts = append(metaParts, "edit")
		}

		log.Infof("Received message %s from %s (%s).", evt.Info.ID, evt.Info.SourceString(), strings.Join(metaParts, ", "))

		am := evt.Message.GetAudioMessage()
		if am != nil {
			audio_data, err := cli.Download(am)
			if err != nil {
				log.Errorf("Failed to download audio: %v", err)
				return
			}
			if am.GetPTT() {
				maybeText := getTranscription(audio_data)
				if maybeText != nil {
					text := *maybeText
					msg := &waProto.Message{
						ExtendedTextMessage: &waProto.ExtendedTextMessage{
							Text: proto.String(*messageHead + text),
							ContextInfo: &waProto.ContextInfo{
								StanzaID:      proto.String(evt.Info.ID),
								Participant:   proto.String(evt.Info.Sender.ToNonAD().String()),
								QuotedMessage: evt.Message,
							},
						},
					}
					_, _ = cli.SendMessage(context.Background(), evt.Info.MessageSource.Chat, msg)
				}
			}
		}
	}
}

// TODO: return error, log in caller
func getTranscription(audio_data []byte) *string {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writer.WriteField("model", "whisper-1")
	writer.WriteField("response_format", "text")
	part, err := writer.CreateFormFile("file", "ptt.oga")
	if err != nil {
		log.Warnf("Transcription: Error creating form file: %#v", err)
		return nil
	}
	_, err = part.Write(audio_data)
	if err != nil {
		log.Warnf("Transcription: Error writing data into part: %#v", err)
		return nil
	}
	err = writer.Close()
	if err != nil {
		log.Warnf("Transcription: Error closing writer: %#v", err)
		return nil
	}
	req, err := http.NewRequest("POST", *apiUrl, body)
	if err != nil {
		log.Warnf("Transcription: Error creating request: %#v", err)
		return nil
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", *apiKey))

	// Send the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf("Transcription: Error sending request: %#v", err)
		return nil
	}
	defer resp.Body.Close()

	// Print the response
	log.Infof("Transcription: Response status: %#v", resp.Status)

	resposeBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Warnf("Transcription: Unable to read response body: %#v", err)
		return nil
	}
	responseText := string(resposeBody)
	if resp.StatusCode != http.StatusOK {
		log.Warnf("Transcription: Got negative response: „%s“", responseText)
	}
	return &responseText
}
