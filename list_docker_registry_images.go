package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)
const(
	TimeOutputLayout = "2006-01-02 15:04:05"
	DataTypeRepoList = "rs"
	DataTypeTagList = "ts"
	DataTypeTagDetail = "td"
)

type JsonTime time.Time

func NewJsonTime(t interface{}) JsonTime {
	return (JsonTime)(t.(time.Time))
}

func (t JsonTime)MarshalJSON() ([]byte, error) {
	stamp := fmt.Sprintf("\"%s\"", time.Time(t).Format(TimeOutputLayout))
	return []byte(stamp), nil
}

func (t JsonTime) After(u JsonTime) bool {
	return (time.Time)(t).After((time.Time)(u))
}

type TagDetail struct {
	Tag string
	Created JsonTime
}

type PayLoad struct {
	Type string
	Repo string
	Tag string
	Target interface{}
}

type Config struct {
	Registries []*Registry `json: "registries"`
}

type Registry struct {
	Alias string 		`json: "alias"`
	Host string 		`json: "host"`
	Port int			`json: "port"`
	Schema string		`json: "schema"`
	Addr string			`json: "addr"`
}

func (conf *Config) findRegistry(alias string) (*Registry, bool) {
	for _, reg := range conf.Registries {
		if strings.EqualFold(reg.Alias, alias) {
			return reg, true
		}
	}
	return nil, false
}

func getForMap(url string) (m map[string]interface{}, err error) {
	res, err := httpClient.Get(url)
	if err != nil {
		return
	}
	defer res.Body.Close()

	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		if err != io.EOF || res.StatusCode < 200 || res.StatusCode >= 300 {
			return
		}
	}

	err = json.Unmarshal(buf, &m)
	if err != nil {
		return
	}
	return
}

func printJson(obj interface{}) {
	j, err := json.MarshalIndent(&obj, "", "   ")
	if err != nil {
		log.Fatalln(err)
	}
	fmt.Println(string(j))
}


// ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
var (
	httpClient *http.Client
	localConf  *Config
	configFilePath string
)

func init() {
	log.SetFlags(log.Lshortfile)
	httpClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ InsecureSkipVerify: true },
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 5 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			IdleConnTimeout:     5 * time.Second,
		},
	}
	configFilePath = fmt.Sprintf("%v/.docker_registry_config.json", os.Getenv("HOME"))
	err := loadConfig(configFilePath)
	if err != nil {
		log.Fatal(err)
	}
}

func initConfig(path string) (err error) {
	defaultConfig := []byte(
		`{
  "registries": [
	{
      "alias": "local",
      "host": "127.0.0.1",
      "port": 5001,
      "schema": "http"
    },
    {
      "alias": "reg01",
      "host": "reg01.example.org",
      "port": 443,
      "schema": "https"
    },
    {
      "alias": "reg01",
      "host": "reg01.example.org",
      "port": 55001,
      "schema": "http"
    }
  ]
}`)
	err = ioutil.WriteFile(path, defaultConfig, 0644)
	return
}

func loadConfig(path string) (err error) {
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		err = initConfig(path)
	}

	f, _ := os.Open(path)
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return
	}
	err = json.Unmarshal(b, &localConf)
	if err != nil {
		return
	}
	for _, reg := range localConf.Registries {
		reg.Addr = fmt.Sprintf("%v://%v:%v", reg.Schema, reg.Host, reg.Port)
	}
	return
}

func fetchRepos(addr string, data chan<- *PayLoad, wg *sync.WaitGroup) {
	defer wg.Done()
	m, err := getForMap(fmt.Sprintf("%v/v2/_catalog", addr))
	if err != nil {
		log.Println(err)
		return
	}
	repos := m["repositories"].([]interface{})
	data <- &PayLoad{
		Type:  	DataTypeRepoList,
		Target: repos,
	}
	wg.Add(len(repos))
}

func fetchTags(addr string, repo string, data chan<- *PayLoad, wg *sync.WaitGroup) {
	defer wg.Done()
	m, err := getForMap(fmt.Sprintf("%v/v2/%v/tags/list", addr, repo))
	if err != nil {
		log.Println(err)
		return
	}
	tags := m["tags"].([]interface{})
	data <- &PayLoad{
		Type:   DataTypeTagList,
		Repo:   repo,
		Target: tags,
	}
	wg.Add(len(tags))
}

func fetchDetailOfTag(addr string, repo string, tag string, data chan<- *PayLoad, wg *sync.WaitGroup) {
	defer wg.Done()
	m, err := getForMap(fmt.Sprintf("%v/v2/%v/manifests/%v", addr, repo, tag))
	if err != nil {
		log.Println(err)
		return
	}
	r := make(map[string]interface{})

	var h []time.Time
	for _, item := range m["history"].([]interface{}){
		i := item.(map[string]interface{})
		str := i["v1Compatibility"].(string)
		var msg map[string]interface{}
		err = json.Unmarshal([]byte(str), &msg)
		if err != nil {
			log.Println(err)
			return
		}

		created, _ := time.Parse(time.RFC3339Nano, msg["created"].(string))
		h = append(h, created)
	}

	// TODO show the creation time of most recent modification(layer), u may implement differently
	sort.Slice(h, func(i, j int) bool {
		return h[i].Before(h[j])
	})
	r["created"] = h[len(h) - 1]

	data <- &PayLoad{
		Type: DataTypeTagDetail,
		Repo: repo,
		Tag: tag,
		Target: r,
	}
}

func getRepoInfo(reg *Registry) map[string] []TagDetail {
	result := make(map[string] []TagDetail)
	var wg sync.WaitGroup
	data := make(chan *PayLoad)
	done := make(chan struct{})

	wg.Add(1)
	go fetchRepos(reg.Addr, data, &wg)

	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	for {
		select {

		case payload := <- data:
			switch payload.Type {
			case DataTypeRepoList:
				for _, repo := range payload.Target.([]interface{}) {
					go fetchTags(reg.Addr, repo.(string), data, &wg)
				}
				break
			case DataTypeTagList:
				for _, tag := range payload.Target.([]interface{}) {
					go fetchDetailOfTag(reg.Addr, payload.Repo, tag.(string), data, &wg)
				}
				break
			case DataTypeTagDetail:
				target := payload.Target.(map[string]interface{})
				_, exits := result[payload.Repo]
				if !exits {
					result[payload.Repo] = make([]TagDetail, 0)
				}
				result[payload.Repo] = append(result[payload.Repo], TagDetail{
					payload.Tag,
					NewJsonTime(target["created"]),
				})
				break
			}

		case <- done:
			close(data)
			for _, tags := range result {
				sort.Slice(tags, func(i, j int) bool {
					return tags[i].Created.After(tags[j].Created) // print tags desc by Created
				})
			}
			return result
		}
	}
}

func main()  {
	if len(os.Args) == 1 {
		log.Fatal("registry alias or addr not defined")
	}
	connectString := os.Args[1]
	reg, ok := localConf.findRegistry(connectString)
	if !ok {
		if !strings.HasPrefix(connectString, "http") {
			connectString = fmt.Sprintf("http://%v", connectString)
		}
		reg = &Registry{ Addr: connectString }
	}
	r := getRepoInfo(reg)
	printJson(r)
}


