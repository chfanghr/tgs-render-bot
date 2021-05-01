package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/go-redis/redis/v8"
	log "github.com/sirupsen/logrus"
	"gopkg.in/tucnak/telebot.v2"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"time"
)

type Config struct {
	BotToken                    string        `json:"bot_token"`
	CacheDir                    string        `json:"cache_dir"`
	Verbose                     bool          `json:"verbose"`
	Proxy                       string        `json:"proxy"`
	RedisUrl                    string        `json:"redis_url"`
	RedisCacheExpireDuration    time.Duration `json:"redis_cache_expire_duration"`
	TgsRenderServiceApiEndpoint string        `json:"tgs_render_service_api_endpoint"`

	redisOptions *redis.Options
}

const defaultCacheDir = "./cache"
const defaultRedisCacheExpireDuration = 5 * time.Minute
const redisUrlEnvKey = "REDIS_URL"
const tgsRenderServiceApiEndpointEnvKey = "RENDER_API"

func NewConfigFromFile(filePath string) (*Config, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file %v: %w", filePath, err)
	}
	//goland:noinspection GoUnhandledErrorResult
	defer file.Close()
	decoder := json.NewDecoder(file)
	config := new(Config)
	err = decoder.Decode(config)
	if err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	if config.CacheDir == "" {
		config.CacheDir = defaultCacheDir
	}

	if config.RedisCacheExpireDuration == 0 {
		config.RedisCacheExpireDuration = defaultRedisCacheExpireDuration
	}

	if config.RedisUrl == "" {
		if envVar := os.Getenv(redisUrlEnvKey); envVar != "" {
			config.RedisUrl = envVar
		} else {
			return nil, fmt.Errorf("failed to detect redis configuration")
		}
	}

	config.redisOptions, err = redis.ParseURL(config.RedisUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to parse redis url: %w", err)
	}

	if config.TgsRenderServiceApiEndpoint == "" {
		if envVar := os.Getenv(tgsRenderServiceApiEndpointEnvKey); envVar != "" {
			config.TgsRenderServiceApiEndpoint = envVar
		} else {
			return nil, fmt.Errorf("failed to detect render service configuration")
		}
	}

	return config, nil
}

type Service struct {
	logger      *log.Logger
	bot         *telebot.Bot
	conf        *Config
	redisClient *redis.Client
}

func isFileOrFolderExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || os.IsExist(err)
}

func NewService(config *Config) (*Service, error) {
	s := &Service{
		logger: log.New(),
		conf:   config,
	}

	var err error
	err = s.makeCacheDirectoryIfNeeded()

	if err != nil {
		s.logger.Errorf("failed to create service: %v", err)
		return nil, err
	}

	err = s.setupBot()
	if err != nil {
		s.logger.Errorf("failed to setup bot: %v", err)
		return nil, err
	}

	err = s.setupRedisClient()
	if err != nil {
		s.logger.Errorf("failed to setup redis client: %v", err)
		return nil, err
	}

	return s, nil
}

const RedisVersionPrefix = "redis_version:"

func (s *Service) setupRedisClient() error {
	s.redisClient = redis.NewClient(s.conf.redisOptions)

	serverInfo, err := s.redisClient.Do(s.redisClient.Context(), "INFO").Result()
	if err != nil {
		return fmt.Errorf("failed to get redis server information: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(serverInfo.(string)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, RedisVersionPrefix) {
			version := strings.TrimPrefix(line, RedisVersionPrefix)
			version = strings.TrimRight(version, "\r\n")
			log.Infof("redis server connected: %v", version)
			break
		}
	}

	return nil
}

func (s *Service) makeCacheDirectoryIfNeeded() error {
	if !isFileOrFolderExists(s.conf.CacheDir) {
		err := os.MkdirAll(s.conf.CacheDir, 0700)
		if err != nil {
			return fmt.Errorf("failed to make cache directory: %w", err)
		}
	}
	return nil
}

func (s *Service) sendMessage(chat *telebot.Chat, message string) {
	_, err := s.bot.Send(chat, message)
	if err != nil {
		s.logger.Errorf("failed to send message: %v", err)
	}
}

func (s *Service) messageFilter(update *telebot.Update) bool {
	s.logger.Infof("telegram message received: %v", update.Message.ID)
	return true
}

const renderCommand = "/render"

func (s *Service) setupBot() error {
	var client *http.Client

	if s.conf.Proxy != "" {
		s.logger.Infof("using proxy: %v", s.conf.Proxy)
		proxyUrl, err := url.Parse(s.conf.Proxy)
		if err != nil {
			return fmt.Errorf("failed to parse proxy url: %w", err)
		}
		client = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyUrl),
			},
		}
	}

	var err error

	poller := telebot.NewMiddlewarePoller(&telebot.LongPoller{
		Limit:   10,
		Timeout: 6 * time.Second,
		AllowedUpdates: []string{
			"message",
		},
	}, s.messageFilter)

	s.bot, err = telebot.NewBot(telebot.Settings{
		Token:   s.conf.BotToken,
		Poller:  poller,
		Verbose: s.conf.Verbose,
		Reporter: func(err error) {
			s.logger.Errorf("telebot: %v", err)
		},
		Client: client,
	})

	if err != nil {
		return fmt.Errorf("failed to create telebot instance: %w", err)
	}

	s.bot.Handle(telebot.OnSticker, s.handleStickerMessage)
	s.bot.Handle(renderCommand, s.handleRenderCommand)

	return nil
}

func getConvertedStickerFilename(sticker *telebot.Sticker) string {
	return sticker.UniqueID + ".gif"
}

func (s *Service) getStickerCachePath(sticker *telebot.Sticker) string {
	return path.Join(s.conf.CacheDir, getConvertedStickerFilename(sticker))
}

func (s *Service) getStickerFromCache(sticker *telebot.Sticker) ([]byte, error) {
	cachePath := s.getStickerCachePath(sticker)

	inMemoryCache, err := s.redisClient.Get(s.redisClient.Context(), cachePath).Result()
	if err != nil {
		s.logger.Infof("failed to find %v in redis cache: %w", cachePath, err)
	} else {
		return []byte(inMemoryCache), nil
	}

	if isFileOrFolderExists(cachePath) {
		data, err := ioutil.ReadFile(cachePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read file: %w", err)
		}
		go s.cacheStickerToRedis(sticker, data)
		return data, nil
	}

	return nil, fmt.Errorf("requested data is not in cache")
}

func (s *Service) handleStickerMessage(message *telebot.Message) {
	if message.Sticker == nil || !message.Sticker.Animated {
		s.sendMessage(message.Chat, "Please send an animated sticker.")
		return
	}
	s.sendMessage(message.Chat, "Your sticker is being processed...")

	convertedFileName := getConvertedStickerFilename(message.Sticker)
	cachedStickerData, err := s.getStickerFromCache(message.Sticker)
	if err != nil {
		s.logger.Infof("failed to get sticker from cache: %v", err)
	} else {
		s.sendConvertedStickerData(message.Chat, cachedStickerData, convertedFileName)
		return
	}

	tgsData, err := s.downloadTgsData(message.Sticker)
	if err != nil {
		s.logger.Errorf("failed to download tgs data: %v", err)
		s.informFailure(message.Chat)
		return
	}

	gifData, err := s.renderTgsToGif(tgsData)
	if err != nil {
		s.logger.Errorf("failed to render tgs: %v", err)
		s.informFailure(message.Chat)
		return
	}

	s.sendConvertedStickerData(message.Chat, gifData, convertedFileName)
	go s.cacheSticker(message.Sticker, gifData)
}

func (s *Service) cacheSticker(sticker *telebot.Sticker, gifData []byte) {
	go s.cacheStickerToRedis(sticker, gifData)

	cachePath := s.getStickerCachePath(sticker)
	err := ioutil.WriteFile(cachePath, gifData, 0600)
	if err != nil {
		s.logger.Warningf("failed to cache %v in filesystem: %w", cachePath, err)
	}
}

func (s *Service) cacheStickerToRedis(sticker *telebot.Sticker, gifData []byte) {
	cachePath := s.getStickerCachePath(sticker)
	_, err := s.redisClient.Set(s.redisClient.Context(), cachePath, gifData, s.conf.RedisCacheExpireDuration).Result()
	if err != nil {
		s.logger.Warningf("failed to cache %v in redis: %v", cachePath, err)
	}
}

func (s *Service) handleRenderCommand(m *telebot.Message) {
	if m.ReplyTo == nil || m.ReplyTo.Sticker == nil || !m.ReplyTo.Sticker.Animated {
		s.sendMessage(m.Chat, "Please reply to an animated sticker message with /render")
		return
	}

	s.handleStickerMessage(m.ReplyTo)
}

type tgsRenderRequest struct {
	Width       uint32  `json:"width"`
	Height      uint32  `json:"height"`
	Fps         float64 `json:"fps"`
	IsTgs       bool    `json:"is_tgs"`
	RlottieData string  `json:"rlottie_data"` // base64 encoded data
}

const DefaultGifWidth = 512
const DefaultGifHeight = 512
const DefaultGifFps = 60.0

func newDefaultTgsRenderRequest(tgsData []byte) *tgsRenderRequest {
	return &tgsRenderRequest{
		Width:       DefaultGifWidth,
		Height:      DefaultGifHeight,
		Fps:         DefaultGifFps,
		IsTgs:       true,
		RlottieData: base64.RawStdEncoding.EncodeToString(tgsData),
	}
}

type tgsRenderError struct {
	Reason string `json:"reason"`
}

type tgsRenderResult struct {
	Error   *tgsRenderError `json:"error"`
	GifData []byte          `json:"gif_data"`
}

func (s *Service) renderTgsToGif(tgsData []byte) ([]byte, error) {
	renderRequest := newDefaultTgsRenderRequest(tgsData)
	jsonRequest, err := json.Marshal(*renderRequest)
	if err != nil {
		s.logger.Fatalf("failed to marshal render request: %v", err)
	}
	req, err := http.NewRequest("POST", s.conf.TgsRenderServiceApiEndpoint, bytes.NewBuffer(jsonRequest))
	if err != nil {
		s.logger.Fatalf("failed to create http request: %v", err)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute render request: %v", err)
	}
	//goland:noinspection GoUnhandledErrorResult
	defer resp.Body.Close()
	jsonDecoder := json.NewDecoder(resp.Body)
	renderResult := new(tgsRenderResult)
	err = jsonDecoder.Decode(renderResult)
	if err != nil {
		return nil, fmt.Errorf("failed to decode render result: %v", err)
	}
	if renderResult.Error != nil {
		return nil, fmt.Errorf("failed to render: %v", renderResult.Error)
	}
	return renderResult.GifData, nil
}

func (s *Service) downloadTgsData(sticker *telebot.Sticker) ([]byte, error) {
	reader, err := s.bot.GetFile(&sticker.File)
	if err != nil {
		return nil, fmt.Errorf("failed to get file: %w", err)
	}
	//goland:noinspection GoUnhandledErrorResult
	defer reader.Close()
	data, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read data: %w", err)
	}
	return data, err
}

func (s *Service) sendConvertedStickerData(chat *telebot.Chat, data []byte, filename string) {
	document := &telebot.Document{
		File: telebot.File{
			FileReader: bytes.NewReader(data),
		},
		FileName: filename,
	}

	_, err := s.bot.Send(chat, document)

	if err != nil {
		s.logger.Errorf("failed to send sticker: %v", err)
		s.informFailure(chat)
	}
}

func (s *Service) informFailure(chat *telebot.Chat) {
	s.sendMessage(chat, "For some reason, your sticker cannot be delivered. Sorry.")
}

func (s *Service) Run() {
	signalChan := make(chan os.Signal)
	signal.Notify(signalChan, os.Interrupt, os.Kill)
	s.bot.Start()
	<-signalChan
	s.bot.Stop()
}

var configFilePath = flag.String("c", "config.json", "path to configuration json file")

func main() {
	flag.Parse()
	config, err := NewConfigFromFile(*configFilePath)
	if err != nil {
		log.Fatalf("failed to load configuration from file: %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		log.Fatalf("failed to create service: %v", err)
	}
	service.Run()
}
