package smugmug

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
)

// maxRetries defines the number of http calls retries (in case of errors) before giving up
const maxRetries = 3
const baseAPIURL = "https://api.smugmug.com"

type header struct {
	name  string
	value string
}

type handler struct {
	oauth *oauthConf
}

func newHTTPHandler(apiKey, apiSecret, userToken, userSecret string) *handler {
	return &handler{
		oauth: newOauthConf(apiKey, apiSecret, userToken, userSecret),
	}
}

// get calls getJSON with the given url
func (s *handler) get(url string, obj interface{}) error {
	if url == "" {
		return errors.New("Can't get empty url")
	}
	return s.getJSON(fmt.Sprintf("%s%s", baseAPIURL, url), obj)
}

// download the resource (image or video) from the given url to the given destination, checking
// if a file with the same size exists (and skipping the download in that case, returning false)
func (s *handler) download(dest, destUnique, downloadURL string, fileSize int64, md5sum string) (bool, error) {
	if _, err := os.Stat(dest); err == nil {
		if sameFileSizes(dest, fileSize) {
			log.Debug("File exists with same size:", downloadURL)
			//if sameFileMD5Sum(dest, md5sum) {
			//	log.Debug("File exists with same md5sum: ", md5sum)
			//}
			return false, nil
		}
		log.Debug("File exists but looks different, using unique identifier")
		dest = destUnique
		log.Debug("Checking if new unique filename exists: ", dest)
		if _, err := os.Stat(dest); err == nil {
			if sameFileSizes(dest, fileSize) {
				log.Debug("Unique file exists with same size:", downloadURL)
				//if sameFileMD5Sum(dest, md5sum) {
				//	log.Debug("Unique fle exists with same md5sum: ", md5sum)
				//}
				return false, nil
			}
		}
	}
	log.Info("Getting ", downloadURL)

	response, err := s.makeAPICall(downloadURL)
	if err != nil {
		return false, fmt.Errorf("%s: download failed with: %s", downloadURL, err)
	}
	defer response.Body.Close()

	// Create empty destination file
	file, err := os.Create(dest)
	if err != nil {
		return false, fmt.Errorf("%s: file creation failed with: %s", dest, err)
	}
	defer file.Close()

	// Copy the content to the file
	_, err = io.Copy(file, response.Body)
	if err != nil {
		return false, fmt.Errorf("%s: file content copy failed with: %s", dest, err)
	}

	log.Info("Saved ", dest)
	return true, nil
}

// getJSON makes a http calls to the given url, trying to decode the JSON response on the given obj
func (s *handler) getJSON(url string, obj interface{}) error {
	var result interface{}
	for i := 1; i <= maxRetries; i++ {
		log.Debug("Calling ", url)
		resp, err := s.makeAPICall(url)
		if err != nil {
			return err
		}
		err = json.NewDecoder(resp.Body).Decode(&obj)
		defer resp.Body.Close()
		if err != nil {
			log.Errorf("%s: reading response. %s", url, err)
			if i >= maxRetries {
				return err
			}
		} else {
			obj = result
			break
		}
	}
	return nil
}

// makeAPICall performs an HTTP call to the given url, returning the response
func (s *handler) makeAPICall(url string) (*http.Response, error) {
	client := &http.Client{}

	var resp *http.Response
	var errorsList []error
	for i := 1; i <= maxRetries; i++ {
		req, err := http.NewRequest("GET", url, nil)

		// Auth header must be generate every time (nonce must change)
		h, err := s.oauth.authorizationHeader(url)
		if err != nil {
			panic(err)
		}
		headers := []header{
			{name: "Accept", value: "application/json"},
			{name: "Authorization", value: h},
		}
		log.Debug(headers)
		addHeaders(req, headers)

		r, err := client.Do(req)
		if err != nil {
			log.Debugf("#%d %s: %s\n", i, url, err)
			errorsList = append(errorsList, err)
			if i >= maxRetries {
				for _, e := range errorsList {
					log.Error(e)
				}
				return nil, errors.New("Too many errors")
			}
			// Go on and try again after a little pause
			time.Sleep(2 * time.Second)
			continue
		}

		if r.StatusCode >= 400 {
			errorsList = append(errorsList, errors.New(r.Status))
			if i >= maxRetries {
				for _, e := range errorsList {
					log.Error(e)
				}
				return nil, errors.New("Too many errors")
			}

			if r.StatusCode == 429 {
				// Header Retry-After tells the number of seconds until the end of the current window
				log.Error("Got 429 too many requests, let's try to wait 10 seconds...")
				log.Errorf("Retry-After header: %s\n", r.Header.Get("Retry-After"))
				time.Sleep(10 * time.Second)
			}
			continue
		}
		resp = r
		break

	}
	return resp, nil
}

// addHeaders to the provided http request
func addHeaders(req *http.Request, headers []header) {
	for _, h := range headers {
		req.Header.Add(h.name, h.value)
	}
}
