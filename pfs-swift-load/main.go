package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/swiftstack/ProxyFS/conf"
)

type workerStruct struct {
	sync.Mutex
	name                          string
	swiftAuthUser                 string
	swiftAuthKey                  string
	swiftAccount                  string
	swiftContainer                string
	swiftContainerStoragePolicy   string
	numThreads                    uint16
	swiftAuthToken                string
	swiftAuthTokenUpdateWaitGroup *sync.WaitGroup
	authorizationsPerformed       uint64
	nextMethodNumberToStart       uint64
	methodsCompleted              uint64
	priorMethodsCompleted         uint64
}

var (
	displayInterval  time.Duration
	optionDestroy    bool
	optionFormat     bool
	swiftMethod      string // One of http.MethodGet, http.MethodHead, http.MethodPut, or http.MethodDelete
	swiftProxyURL    string
	swiftPutBuffer   []byte
	swiftPutSize     uint64
	workerArray      []*workerStruct
	workerMap        map[string]*workerStruct
	workersGoGate    sync.WaitGroup
	workersReadyGate sync.WaitGroup
)

func main() {
	var (
		args                   []string
		confMap                conf.ConfMap
		err                    error
		infoResponse           *http.Response
		methodsCompleted       uint64
		methodsCompletedString string
		methodsDelta           uint64
		methodsDeltaString     string
		optionString           string
		worker                 *workerStruct
		workerList             []string
		workerName             string
		workerSectionName      string
	)

	// Parse arguments

	args = os.Args[1:]

	// Check that os.Args[1] was supplied... it might be a .conf or an option list (followed by a .conf)
	if 0 == len(args) {
		log.Fatalf("no .conf file specified")
	}

	optionDestroy = false
	optionFormat = false

	if "-" == args[0][:1] {
		// Peel off option list from args
		optionString = args[0]
		args = args[1:]

		// Check that os.Args[2]-specified (and required) .conf was supplied
		if 0 == len(args) {
			log.Fatalf("no .conf file specified")
		}

		switch optionString {
		case "-F":
			optionFormat = true
			optionDestroy = false
		case "-D":
			optionFormat = false
			optionDestroy = true
		case "-DF":
			optionFormat = true
			optionDestroy = true
		case "-FD":
			optionFormat = true
			optionDestroy = true
		default:
			log.Fatalf("unexpected option supplied: %s", optionString)
		}
	} else {
		optionDestroy = false
		optionFormat = false
	}

	confMap, err = conf.MakeConfMapFromFile(args[0])
	if nil != err {
		log.Fatalf("failed to load config: %v", err)
	}

	// Update confMap with any extra os.Args supplied
	err = confMap.UpdateFromStrings(args[1:])
	if nil != err {
		log.Fatalf("failed to load config overrides: %v", err)
	}

	// Process resultant confMap

	if optionDestroy && !optionFormat {
		// SwiftMethod not required if we are only going to optionDestoy (and exit)
	} else {
		swiftMethod, err = confMap.FetchOptionValueString("LoadParameters", "SwiftMethod")
		if nil != err {
			log.Fatalf("confMap.FetchOptionValueString(\"LoadParameters\", \"SwiftMethod\") failed: %v", err)
		}
		switch swiftMethod {
		case http.MethodGet:
			// Nothing extra to do here
		case http.MethodHead:
			// Nothing extra to do here
		case http.MethodPut:
			swiftPutSize, err = confMap.FetchOptionValueUint64("LoadParameters", "SwiftPutSize")
			if nil != err {
				return
			}
			swiftPutBuffer = make([]byte, swiftPutSize)
		case http.MethodDelete:
			// Nothing extra to do here
		default:
			log.Fatalf("SwiftMethod: %s not supported", swiftMethod)
		}
	}

	swiftProxyURL, err = confMap.FetchOptionValueString("LoadParameters", "SwiftProxy")
	if nil != err {
		log.Fatalf("confMap.FetchOptionValueString(\"LoadParameters\", \"SwiftProxy\") failed: %v", err)
	}

	workerList, err = confMap.FetchOptionValueStringSlice("LoadParameters", "WorkerList")
	if nil != err {
		log.Fatalf("confMap.FetchOptionValueStringSlice(\"LoadParameters\", \"WorkerList\") failed: %v", err)
	}
	if 0 == len(workerList) {
		log.Fatalf("WorkerList must not be empty")
	}

	displayInterval, err = confMap.FetchOptionValueDuration("LoadParameters", "DisplayInterval")
	if nil != err {
		log.Fatalf("confMap.FetchOptionValueDuration(\"LoadParameters\", \"DisplayInterval\") failed: %v", err)
	}

	infoResponse, err = http.Get(swiftProxyURL + "info")
	if nil != err {
		log.Fatalf("http.Get(\"%sinfo\") failed: %v", swiftProxyURL, err)
	}
	_, err = ioutil.ReadAll(infoResponse.Body)
	infoResponse.Body.Close()
	if nil != err {
		log.Fatalf("ioutil.ReadAll(infoResponse.Body) failed: %v", err)
	}

	workerArray = make([]*workerStruct, 0, len(workerList))
	workerMap = make(map[string]*workerStruct)

	workersGoGate.Add(1)

	for _, workerName = range workerList {
		worker = &workerStruct{
			name:                          workerName,
			swiftAuthToken:                "",
			swiftAuthTokenUpdateWaitGroup: nil,
			authorizationsPerformed:       0,
			nextMethodNumberToStart:       0,
			methodsCompleted:              0,
			priorMethodsCompleted:         0,
		}

		workerSectionName = "Worker:" + workerName

		worker.swiftAuthUser, err = confMap.FetchOptionValueString(workerSectionName, "SwiftUser")
		if nil != err {
			log.Fatalf("confMap.FetchOptionValueString(\"%s\", \"SwiftUser\") failed: %v", workerSectionName, err)
		}
		worker.swiftAuthKey, err = confMap.FetchOptionValueString(workerSectionName, "SwiftKey")
		if nil != err {
			log.Fatalf("confMap.FetchOptionValueString(\"%s\", \"SwiftKey\") failed: %v", workerSectionName, err)
		}
		worker.swiftAccount, err = confMap.FetchOptionValueString(workerSectionName, "SwiftAccount")
		if nil != err {
			log.Fatalf("confMap.FetchOptionValueString(\"%s\", \"SwiftAccount\") failed: %v", workerSectionName, err)
		}
		worker.swiftContainer, err = confMap.FetchOptionValueString(workerSectionName, "SwiftContainer")
		if nil != err {
			log.Fatalf("confMap.FetchOptionValueString(\"%s\", \"SwiftContainer\") failed: %v", workerSectionName, err)
		}
		worker.numThreads, err = confMap.FetchOptionValueUint16(workerSectionName, "NumThreads")
		if nil != err {
			log.Fatalf("confMap.FetchOptionValueUint16(\"%s\", \"NumThreads\") failed: %v", workerSectionName, err)
		}

		if optionFormat {
			worker.swiftContainerStoragePolicy, err = confMap.FetchOptionValueString(workerSectionName, "SwiftContainerStoragePolicy")
			if nil != err {
				log.Fatalf("confMap.FetchOptionValueString(\"%s\", \"SwiftContainerStoragePolicy\") failed: %v", workerSectionName, err)
			}
		}

		workerArray = append(workerArray, worker)
		workerMap[workerName] = worker

		workersReadyGate.Add(1)

		go worker.workerLauncher()
	}

	workersReadyGate.Wait()

	if optionDestroy && !optionFormat {
		// We are just going to exit if all we were asked to do was optionDestroy

		os.Exit(0)
	}

	// Print workerName's as column heads

	fmt.Printf("       ")

	for _, worker = range workerArray {
		fmt.Printf("     %-20s", worker.name)
	}

	fmt.Println()

	// Workers (or, rather, all their Threads) are ready to go... so kick off the test

	workersGoGate.Done()

	for {
		time.Sleep(displayInterval)

		for _, worker = range workerArray {
			worker.Lock()
			methodsCompleted = worker.methodsCompleted
			methodsDelta = methodsCompleted - worker.priorMethodsCompleted
			worker.priorMethodsCompleted = methodsCompleted
			worker.Unlock()

			methodsCompletedString = fmt.Sprintf("%d", methodsCompleted)
			methodsDeltaString = fmt.Sprintf("(+%d)", methodsDelta)

			fmt.Printf("  %12s %-10s", methodsCompletedString, methodsDeltaString)
		}

		fmt.Println()
	}
}

func (worker *workerStruct) workerLauncher() {
	var (
		containerDeleteRequest  *http.Request
		containerDeleteResponse *http.Response
		containerGetBody        []byte
		containerGetRequest     *http.Request
		containerGetResponse    *http.Response
		containerPutRequest     *http.Request
		containerPutResponse    *http.Response
		containerURL            string
		err                     error
		httpClient              *http.Client
		objectDeleteRequest     *http.Request
		objectDeleteResponse    *http.Response
		objectList              []string
		objectName              string
		objectURL               string
		swiftAuthToken          string
		threadIndex             uint16
	)

	worker.updateSwiftAuthToken()

	httpClient = &http.Client{}

	containerURL = fmt.Sprintf("%sv1/%s/%s", swiftProxyURL, worker.swiftAccount, worker.swiftContainer)

	if optionDestroy {
		// First, empty the containerURL

	optionDestroyLoop:
		for {
			swiftAuthToken = worker.fetchSwiftAuthToken()
			containerGetRequest, err = http.NewRequest("GET", containerURL, nil)
			if nil != err {
				log.Fatalf("http.NewRequest(\"GET\", \"%s\", nil) failed: %v", containerURL, err)
			}
			containerGetRequest.Header.Set("X-Auth-Token", swiftAuthToken)
			containerGetResponse, err = httpClient.Do(containerGetRequest)
			if nil != err {
				log.Fatalf("httpClient.Do(containerGetRequest) failed: %v", err)
			}
			containerGetBody, err = ioutil.ReadAll(containerGetResponse.Body)
			containerGetResponse.Body.Close()
			if nil != err {
				log.Fatalf("ioutil.ReadAll(containerGetResponse.Body) failed: %v", err)
			}

			switch containerGetResponse.StatusCode {
			case http.StatusOK:
				objectList = strings.Split(string(containerGetBody[:]), "\n")

				if 0 == len(objectList) {
					break optionDestroyLoop
				}

				for _, objectName = range objectList {
					objectURL = containerURL + "/" + objectName

				objectDeleteRetryLoop:
					for {
						swiftAuthToken = worker.fetchSwiftAuthToken()
						objectDeleteRequest, err = http.NewRequest("DELETE", objectURL, nil)
						if nil != err {
							log.Fatalf("http.NewRequest(\"DELETE\", \"%s\", nil) failed: %v", objectURL, err)
						}
						objectDeleteRequest.Header.Set("X-Auth-Token", swiftAuthToken)
						objectDeleteResponse, err = httpClient.Do(objectDeleteRequest)
						if nil != err {
							log.Fatalf("httpClient.Do(objectDeleteRequest) failed: %v", err)
						}
						_, err = ioutil.ReadAll(objectDeleteResponse.Body)
						objectDeleteResponse.Body.Close()
						if nil != err {
							log.Fatalf("ioutil.ReadAll(objectDeleteResponse.Body) failed: %v", err)
						}

						switch objectDeleteResponse.StatusCode {
						case http.StatusNoContent:
							break objectDeleteRetryLoop
						case http.StatusNotFound:
							// Object apparently already deleted... Container just doesn't know it yet
							break objectDeleteRetryLoop
						case http.StatusUnauthorized:
							worker.updateSwiftAuthToken()
						default:
							log.Fatalf("DELETE response had unexpected status: %s (%d)", objectDeleteResponse.Status, objectDeleteResponse.StatusCode)
						}
					}
				}
			case http.StatusNoContent:
				break optionDestroyLoop
			case http.StatusNotFound:
				// Container apparently already deleted...
				break optionDestroyLoop
			default:
				log.Fatalf("GET response had unexpected status: %s (%d)", containerGetResponse.Status, containerGetResponse.StatusCode)
			}
		}

		// Now, delete containerURL

	containerDeleteRetryLoop:
		for {
			swiftAuthToken = worker.fetchSwiftAuthToken()
			containerDeleteRequest, err = http.NewRequest("DELETE", containerURL, nil)
			if nil != err {
				log.Fatalf("http.NewRequest(\"DELETE\", \"%s\", nil) failed: %v", containerURL, err)
			}
			containerDeleteRequest.Header.Set("X-Auth-Token", swiftAuthToken)
			containerDeleteResponse, err = httpClient.Do(containerDeleteRequest)
			if nil != err {
				log.Fatalf("httpClient.Do(containerDeleteRequest) failed: %v", err)
			}
			_, err = ioutil.ReadAll(containerDeleteResponse.Body)
			containerDeleteResponse.Body.Close()
			if nil != err {
				log.Fatalf("ioutil.ReadAll(containerDeleteResponse.Body) failed: %v", err)
			}

			switch containerDeleteResponse.StatusCode {
			case http.StatusNoContent:
				break containerDeleteRetryLoop
			case http.StatusNotFound:
				// Container apparently already deleted...
				break containerDeleteRetryLoop
			case http.StatusUnauthorized:
				worker.updateSwiftAuthToken()
			default:
				log.Fatalf("DELETE response had unexpected status: %s (%d)", containerDeleteResponse.Status, containerDeleteResponse.StatusCode)
			}
		}

		if !optionFormat {
			// We are just going to exit if all we were asked to do was optionDestroy
			workersReadyGate.Done()
			return
		}
	}

	if optionFormat {
	containerPutRetryLoop:
		for {
			swiftAuthToken = worker.fetchSwiftAuthToken()
			containerPutRequest, err = http.NewRequest("PUT", containerURL, nil)
			if nil != err {
				log.Fatalf("http.NewRequest(\"PUT\", \"%s\", nil) failed: %v", containerURL, err)
			}
			containerPutRequest.Header.Set("X-Auth-Token", swiftAuthToken)
			containerPutRequest.Header.Set("X-Storage-Policy", worker.swiftContainerStoragePolicy)
			containerPutResponse, err = httpClient.Do(containerPutRequest)
			if nil != err {
				log.Fatalf("httpClient.Do(containerPutRequest) failed: %v", err)
			}
			_, err = ioutil.ReadAll(containerPutResponse.Body)
			containerPutResponse.Body.Close()
			if nil != err {
				log.Fatalf("ioutil.ReadAll(containerPutResponse.Body) failed: %v", err)
			}

			switch containerPutResponse.StatusCode {
			case http.StatusCreated:
				break containerPutRetryLoop
			case http.StatusNoContent:
				worker.updateSwiftAuthToken()
			default:
				log.Fatalf("PUT response had unexpected status: %s (%d)", containerPutResponse.Status, containerPutResponse.StatusCode)
			}
		}
	}

	for threadIndex = 0; threadIndex < worker.numThreads; threadIndex++ {
		workersReadyGate.Add(1)

		go worker.workerThreadLauncher()
	}

	workersReadyGate.Done()

	workersGoGate.Wait()
}

func (worker *workerStruct) workerThreadLauncher() {
	var (
		err                 error
		httpClient          *http.Client
		methodNumberToStart uint64
		methodRequest       *http.Request
		methodResponse      *http.Response
		swiftAuthToken      string
		swiftPutReader      *bytes.Reader
		url                 string
	)

	httpClient = &http.Client{}

	workersReadyGate.Done()

	workersGoGate.Wait()

	methodNumberToStart = worker.fetchNextMethodNumberToStart()

	for {
		url = fmt.Sprintf("%sv1/%s/%s/%016X", swiftProxyURL, worker.swiftAccount, worker.swiftContainer, methodNumberToStart)

		swiftAuthToken = worker.fetchSwiftAuthToken()

		switch swiftMethod {
		case http.MethodGet:
			methodRequest, err = http.NewRequest("GET", url, nil)
			if nil != err {
				log.Fatalf("http.NewRequest(\"GET\", \"%s\", nil) failed: %v", url, err)
			}
		case http.MethodHead:
			methodRequest, err = http.NewRequest("HEAD", url, nil)
			if nil != err {
				log.Fatalf("http.NewRequest(\"HEAD\", \"%s\", nil) failed: %v", url, err)
			}
		case http.MethodPut:
			swiftPutReader = bytes.NewReader(swiftPutBuffer)
			methodRequest, err = http.NewRequest("PUT", url, swiftPutReader)
			if nil != err {
				log.Fatalf("http.NewRequest(\"PUT\", \"%s\", swiftPutReader) failed: %v", url, err)
			}
		case http.MethodDelete:
			methodRequest, err = http.NewRequest("DELETE", url, nil)
			if nil != err {
				log.Fatalf("http.NewRequest(\"DELETE\", \"%s\", nil) failed: %v", url, err)
			}
		default:
			log.Fatalf("SwiftMethod: %s not supported", swiftMethod)
		}

		methodRequest.Header.Set("X-Auth-Token", swiftAuthToken)
		methodResponse, err = httpClient.Do(methodRequest)
		if nil != err {
			log.Fatalf("httpClient.Do(methodRequest) failed: %v", err)
		}
		_, err = ioutil.ReadAll(methodResponse.Body)
		methodResponse.Body.Close()
		if nil != err {
			log.Fatalf("ioutil.ReadAll(methodResponse.Body) failed: %v", err)
		}
		switch methodResponse.StatusCode {
		case http.StatusOK:
			worker.incMethodsCompleted()
			methodNumberToStart = worker.fetchNextMethodNumberToStart()
		case http.StatusNoContent:
			worker.incMethodsCompleted()
			methodNumberToStart = worker.fetchNextMethodNumberToStart()
		case http.StatusNotFound:
			methodNumberToStart = worker.fetchNextMethodNumberToStart()
		case http.StatusCreated:
			worker.incMethodsCompleted()
			methodNumberToStart = worker.fetchNextMethodNumberToStart()
		case http.StatusUnauthorized:
			worker.updateSwiftAuthToken()
		default:
			log.Fatalf("%s response for url \"%s\" had unexpected status: %s (%d)", swiftMethod, url, methodResponse.Status, methodResponse.StatusCode)
		}
	}
}

func (worker *workerStruct) fetchNextMethodNumberToStart() (methodNumberToStart uint64) {
	worker.Lock()
	methodNumberToStart = worker.nextMethodNumberToStart
	worker.nextMethodNumberToStart = methodNumberToStart + 1
	worker.Unlock()
	return
}

func (worker *workerStruct) incMethodsCompleted() {
	worker.Lock()
	worker.methodsCompleted++
	worker.Unlock()
}

func (worker *workerStruct) fetchSwiftAuthToken() (swiftAuthToken string) {
	var (
		swiftAuthTokenUpdateWaitGroup *sync.WaitGroup
	)

	for {
		worker.Lock()

		swiftAuthTokenUpdateWaitGroup = worker.swiftAuthTokenUpdateWaitGroup

		if nil == swiftAuthTokenUpdateWaitGroup {
			swiftAuthToken = worker.swiftAuthToken
			worker.Unlock()
			return
		}

		worker.Unlock()

		swiftAuthTokenUpdateWaitGroup.Wait()
	}
}

func (worker *workerStruct) updateSwiftAuthToken() {
	var (
		err         error
		getRequest  *http.Request
		getResponse *http.Response
		httpClient  *http.Client
	)

	worker.Lock()

	if nil != worker.swiftAuthTokenUpdateWaitGroup {
		worker.Unlock()

		_ = worker.fetchSwiftAuthToken()

		return
	}

	worker.swiftAuthTokenUpdateWaitGroup = &sync.WaitGroup{}
	worker.swiftAuthTokenUpdateWaitGroup.Add(1)

	worker.Unlock()

	getRequest, err = http.NewRequest("GET", swiftProxyURL+"auth/v1.0", nil)
	if nil != err {
		log.Fatalf("http.NewRequest(\"GET\", \"%sauth/v1.0\", nil) failed: %v", swiftProxyURL, err)
	}

	getRequest.Header.Set("X-Auth-User", worker.swiftAuthUser)
	getRequest.Header.Set("X-Auth-Key", worker.swiftAuthKey)

	httpClient = &http.Client{}

	getResponse, err = httpClient.Do(getRequest)
	if nil != err {
		log.Fatalf("httpClient.Do(getRequest) failed: %v", err)
	}
	_, err = ioutil.ReadAll(getResponse.Body)
	getResponse.Body.Close()
	if nil != err {
		log.Fatalf("ioutil.ReadAll(getResponse.Body) failed: %v", err)
	}

	worker.Lock()

	worker.swiftAuthToken = getResponse.Header.Get("X-Auth-Token")

	worker.authorizationsPerformed++

	worker.swiftAuthTokenUpdateWaitGroup.Done()

	worker.swiftAuthTokenUpdateWaitGroup = nil

	worker.Unlock()
}
