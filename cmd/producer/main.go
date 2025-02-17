/*
Copyright 2020 The Knative Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/bradleypeabody/gouuidv6"

	"github.com/go-redis/redis/v8"
	"github.com/kelseyhightower/envconfig"
)

// Request size limit in bytes.
const bytesInMB = 1000000

type envInfo struct {
	StreamName       string `envconfig:"REDIS_STREAM_NAME"`
	RedisAddress     string `envconfig:"REDIS_ADDRESS"`
	RequestSizeLimit int64  `envconfig:"REQUEST_SIZE_LIMIT"`
	TlsCert          string `envconfig:"TLS_CERT"`
}

type requestData struct {
	ID        string              `json:"id"`
	ReqURL    string              `json:"url"`
	ReqBody   string              `json:"body"`
	ReqHeader map[string][]string `json:"header"`
	ReqMethod string              `json:"method"`
}

type redisInterface interface {
	write(ctx context.Context, s envInfo, reqJSON []byte, id string) error
}

type myRedis struct {
	client redis.Cmdable
}

type TLSConfig struct {
	TLSCertificate string
}

var env envInfo
var rc redisInterface
var now = time.Now

func main() {
	// Get env info for queue.
	err := envconfig.Process("", &env)
	if err != nil {
		log.Fatal(err.Error())
	}

	rc = setUpRedis()

	// Start an HTTP Server,
	http.HandleFunc("/", handleRequest)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func setUpRedis() redisInterface {
	// set up redis client
	roots := x509.NewCertPool()

	// AppendCertsFromPEM reports whether a cert was parsed
	certFound := roots.AppendCertsFromPEM([]byte(env.TlsCert))

	if certFound {
		log.Println("found a cert")
		opt, err := redis.ParseURL(env.RedisAddress)
		if err != nil {
			log.Fatal(err.Error())
		}
		opt.TLSConfig = &tls.Config{
			RootCAs: roots,
		}
		rc = &myRedis{
			client: redis.NewClient(opt),
		}
	} else {
		log.Println("didnt find a cert")
		rc = &myRedis{
			client: redis.NewClient(&redis.Options{
				Addr:     env.RedisAddress,
				Password: "",
				DB:       0,
			}),
		}
	}
	return rc
}

// Handle requests coming to producer service by error checking and writing to storage.
func handleRequest(w http.ResponseWriter, r *http.Request) {
	// Check that body length doesn't exceed limit.
	r.Body = http.MaxBytesReader(w, r.Body, env.RequestSizeLimit)
	// read the request body
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			log.Println("HTTP Request body too large ", err)
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			log.Println("Error writing to buffer: ", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	reqBodyString := string(b)
	id := gouuidv6.NewFromTime(now()).String()
	originalHost := r.Header.Get("Async-Original-Host")
	reqData := requestData{
		ID:        id,
		ReqBody:   reqBodyString,
		ReqURL:    "http://" + originalHost + r.URL.String(),
		ReqHeader: r.Header,
		ReqMethod: r.Method,
	}
	reqJSON, err := json.Marshal(reqData)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("Failed to marshal request: ", err)
		return
	}

	// Write the request information to the storage.
	if err = rc.write(r.Context(), env, reqJSON, reqData.ID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("Error asynchronous writing request to storage ", err)
		return
	}
	log.Println("request accepted")
	w.WriteHeader(http.StatusAccepted)
	return
}

// Function to write to Redis stream.
func (mr *myRedis) write(ctx context.Context, s envInfo, reqJSON []byte, id string) (err error) {
	strCMD := mr.client.XAdd(ctx, &redis.XAddArgs{
		Stream: s.StreamName,
		Values: map[string]interface{}{
			"data": reqJSON,
		},
	})
	if strCMD.Err() != nil {
		return fmt.Errorf("failed to publish %q: %v", id, strCMD.Err())
	}
	return
}
