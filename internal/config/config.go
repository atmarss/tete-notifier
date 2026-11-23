package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	APIToken        string
	DBHost          string
	DBUser          string
	DBPass          string
	DBName          string
	DBPort          int
	MQTTHost        string
	MQTTPort        int
	MQTTUser        string
	MQTTPass        string
	CarID           int
	LogLevel        string
	PushDebounceSec int // 推送防抖初始时间，后续会进行3次指数退避重试，按(次数-1)倍增加

	// 车辆启动通知：P 档 -> D 档后持续 D 档 StartConfirmSeconds 秒才推送
	StartNotifyEnabled     bool // 是否开启车辆启动通知
	StartNotifyCooldownSec int  // 启动通知冷却时间，避免短时间内重复推送
	StartConfirmSeconds    int  // P->D 后需持续 D 档的确认秒数

	// 车门打开通知
	DoorNotifyEnabled      bool // 是否开启车门打开通知
	DoorNotifyCooldownSec  int  // 开门通知冷却时间，避免短时间内重复推送
	DoorNotifyPDurationSec int  // 需持续 P 档多久后开门才通知（秒），避免刚停车正常下车误报
}

func Load() *Config {
	_ = godotenv.Load()

	return &Config{
		APIToken:               os.Getenv("API_TOKEN"),
		DBHost:                 getEnv("DATABASE_HOST", "database"),
		DBUser:                 getEnv("DATABASE_USER", "teslamate"),
		DBPass:                 os.Getenv("DATABASE_PASS"),
		DBName:                 getEnv("DATABASE_NAME", "teslamate"),
		DBPort:                 mustInt(os.Getenv("DATABASE_PORT"), 5432),
		MQTTHost:               getEnv("MQTT_HOST", "mosquitto"),
		MQTTPort:               mustInt(os.Getenv("MQTT_PORT"), 1883),
		MQTTUser:               os.Getenv("MQTT_USER"),
		MQTTPass:               os.Getenv("MQTT_PASS"),
		CarID:                  mustInt(os.Getenv("CAR_ID"), 1),
		LogLevel:               getEnv("LOG_LEVEL", "info"),
		PushDebounceSec:        mustInt(os.Getenv("PUSH_DEBOUNCE_SECONDS"), 5),
		StartNotifyEnabled:     mustBool(os.Getenv("ENABLE_START_NOTIFY"), true),
		StartNotifyCooldownSec: mustInt(os.Getenv("START_NOTIFY_COOLDOWN_SECONDS"), 60),
		StartConfirmSeconds:    mustInt(os.Getenv("START_CONFIRM_SECONDS"), 10),
		DoorNotifyEnabled:      mustBool(os.Getenv("ENABLE_DOOR_NOTIFY"), true),
		DoorNotifyCooldownSec:  mustInt(os.Getenv("DOOR_NOTIFY_COOLDOWN_SECONDS"), 60),
		DoorNotifyPDurationSec: mustInt(os.Getenv("DOOR_NOTIFY_P_DURATION_SECONDS"), 300),
	}
}

func mustInt(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustBool(s string, def bool) bool {
	if s == "" {
		return def
	}
	if v, err := strconv.ParseBool(s); err == nil {
		return v
	}
	return def
}
