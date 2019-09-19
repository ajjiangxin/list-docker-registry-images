package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	httpClient  = &http.Client{ Timeout: time.Second * 10 }
	reqCounter int32
	localConf  *Config
)

func init() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	reqCounter = int32(0)
	err := loadConfig()
	if err != nil {
		log.Println(err)
		return
	}
}

func getAsMap(url string) (m map[string]interface{}, err error) {
	atomic.AddInt32(&reqCounter, 1)
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

type PayLoad struct {
	Type string
	Repo string
	Tag string
	Target interface{}
}

func fetchRepos(addr string, c chan<- *PayLoad, wg *sync.WaitGroup) {
	defer wg.Done()
	m, err := getAsMap(fmt.Sprintf("%v/v2/_catalog", addr))
	if err != nil {
		log.Println(err)
		return
	}
	repos := m["repositories"].([]interface{})
	c <- &PayLoad{
		Type:   "repos",
		Target: repos,
	}
	wg.Add(len(repos))
}

func fetchTags(addr string, repo string, c chan<- *PayLoad, wg *sync.WaitGroup) {
	defer wg.Done()
	m, err := getAsMap(fmt.Sprintf("%v/v2/%v/tags/list", addr, repo))
	if err != nil {
		log.Println(err)
		return
	}
	tags := m["tags"].([]interface{})
	c <- &PayLoad{
		Type:   "tags",
		Repo:   repo,
		Target: tags,
	}
	wg.Add(len(tags))
}

func fetchDetailOfTag(addr string, repo string, tag string, c chan<- *PayLoad, wg *sync.WaitGroup) {
	defer wg.Done()
	m, err := getAsMap(fmt.Sprintf("%v/v2/%v/manifests/%v", addr, repo, tag))
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

		// time.RFC3339Nano: "2019-02-17T10:10:13.132136671Z"
		created, _ := time.Parse(time.RFC3339Nano, msg["created"].(string))
		h = append(h, created)
	}
	sort.Slice(h, func(i, j int) bool {
		return h[i].Before(h[j])
	})

	r["created"] = h[len(h) - 1]

	c <- &PayLoad{
		Type: "tagDetail",
		Repo: repo,
		Tag: tag,
		Target: r,
	}
}

type JsonTime time.Time

func NewJsonTime(t interface{}) JsonTime {
	return (JsonTime)(t.(time.Time))
}

func (t JsonTime)MarshalJSON() ([]byte, error) {
	stamp := fmt.Sprintf("\"%s\"", time.Time(t).Format("2006-01-02 15:04:05"))
	return []byte(stamp), nil
}

type TagDetail struct {
	Tag string
	Created JsonTime
}

func getRepoInfo(reg *Registry) interface{} {

	result := make(map[string] []TagDetail)

	var wg sync.WaitGroup
	c := make(chan *PayLoad)
	done := make(chan struct{})

	wg.Add(1)
	go fetchRepos(reg.Addr, c, &wg)

	go func() {
		wg.Wait()
		done <- struct{}{}
	}()


	for {
		select {
		case payload := <-c:
			switch payload.Type {
			case "repos":
				for _, repo := range payload.Target.([]interface{}) {
					go fetchTags(reg.Addr, repo.(string), c, &wg)
				}
				break
			case "tags":
				for _, tag := range payload.Target.([]interface{}) {
					go fetchDetailOfTag(reg.Addr, payload.Repo, tag.(string), c, &wg)
				}
				break
			case "tagDetail":
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
			close(c)
			for _, tags := range result {
				sort.Slice(tags, func(i, j int) bool {
					return (time.Time)(tags[i].Created).After((time.Time)(tags[j].Created)) // print tags desc by Created
				})
			}
			return result
		}
	}
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

func loadConfig() (err error) {
	f, err := os.Open(fmt.Sprintf("%v/.regman/config.json", os.Getenv("HOME")))
	if err != nil {
		return
	}
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


func main()  {
	var repoInfo interface{}
	defer func() func(){
		start := time.Now()
		return func() {
			elapsed := time.Since(start)
			printJson(repoInfo)
			log.Printf("req: %d, elapsed %v\n", reqCounter, elapsed.Round(1000000))
		}
	}()()
	args := os.Args[1:]
	if len(args) == 0 {
		panic("registry alias or addr not found")
	}
	reg, ok := localConf.findRegistry(args[0])
	if !ok {
		addr := args[0]
		if !strings.HasPrefix(addr, "http") {
			addr = fmt.Sprintf("http://%v", addr)
		}
		reg = &Registry{
			Addr: addr,
		}
	}
	repoInfo = getRepoInfo(reg)
}


