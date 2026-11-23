package mqtt

import (
	"fmt"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atmarss/tete-notifier/internal/config"
	"github.com/atmarss/tete-notifier/internal/db"
	"github.com/atmarss/tete-notifier/internal/models"
	"github.com/atmarss/tete-notifier/internal/notifier"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Client struct {
	cfg                *config.Config
	client             mqtt.Client
	carName            string
	lastDriveID        uint
	lastChargeID       uint
	lastShiftState     string
	lastUserPresent    bool
	lastChargingState  string
	lastBatteryLevel   float64
	lastIdealRangeKM   float64
	lastDriverDoorOpen bool
	lastLocked         bool
	lastState          string
	chargeLimitSoc     int

	lastStartNotifyTime  time.Time // 上次启动通知时间，用于冷却控制
	lastDoorNotifyTime   time.Time // 上次车门通知时间，用于冷却控制
	lastUnlockNotifyTime time.Time // 上次忘记锁车通知时间，用于冷却控制
	pGearSince           time.Time // 进入 P 档的时间，用于判断"长时间停放"

	mu                sync.Mutex
	retryMu           sync.Mutex // 单独保护重试计数，避免与 c.mu 死锁
	debounceTimer     *time.Timer
	stateSettleTimer  *time.Timer
	startConfirmTimer *time.Timer // P->D 后确认 10 秒仍为 D 的计时器
	unlockCheckTimer  *time.Timer // 未锁状态延时复核的计时器

	retryCountDrive  int
	retryCountCharge int
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		cfg:               cfg,
		lastDriveID:       0,
		lastChargeID:      0,
		lastUserPresent:   true,
		lastChargingState: "",
		lastBatteryLevel:  0,
		lastIdealRangeKM:  0,
		lastState:         "",
	}
}

func (c *Client) Connect() error {
	broker := fmt.Sprintf("tcp://%s:%d", c.cfg.MQTTHost, c.cfg.MQTTPort)

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetUsername(c.cfg.MQTTUser).
		SetPassword(c.cfg.MQTTPass).
		SetClientID(fmt.Sprintf("tete-notifier-%d-%d", c.cfg.CarID, time.Now().Unix())).
		SetAutoReconnect(true).
		SetKeepAlive(2 * time.Minute).
		SetCleanSession(true).
		SetConnectRetry(true)

	opts.OnConnect = func(client mqtt.Client) {
		log.Println("✅ MQTT 已连接")
		name, err := db.GetCarName(c.cfg.CarID)
		c.mu.Lock()
		if err == nil && name != "" {
			c.carName = name
		} else {
			c.carName = "我的 Tesla"
		}
		c.mu.Unlock()

		base := fmt.Sprintf("teslamate/cars/%d", c.cfg.CarID)
		topics := []string{
			base + "/state",
			base + "/shift_state",
			base + "/is_user_present",
			base + "/charging_state",
			base + "/battery_level",
			base + "/ideal_battery_range_km",
			base + "/driver_front_door_open",
			base + "/locked",
			base + "/charge_limit_soc",
		}

		for _, t := range topics {
			if token := client.Subscribe(t, 0, c.messageHandler); token.Wait() && token.Error() != nil {
				log.Printf("❌ 订阅失败 %s: %v", t, token.Error())
			}
		}

		// 发送服务启动通知
		title := "🔔 推送服务通知"
		content := fmt.Sprintf("🚗 %s 通知推送服务已开启 ✅", c.carName)
		if c.cfg.APIToken != "" {
			go func() {
				if err := notifier.SendNotification(c.cfg.APIToken, title, content); err != nil {
					log.Printf("❌ 服务启动通知推送失败: %v", err)
				} else {
					log.Printf("✅ 服务启动通知已推送")
				}
			}()
		}
	}

	c.client = mqtt.NewClient(opts)
	token := c.client.Connect()
	if token.Wait() && token.Error() != nil {
		return token.Error()
	}
	return nil
}

func (c *Client) Disconnect() {
	if c.client != nil && c.client.IsConnected() {
		c.client.Disconnect(250)
	}
}

func (c *Client) messageHandler(client mqtt.Client, msg mqtt.Message) {
	topic := msg.Topic()
	payload := string(msg.Payload())

	// 是否需要触发业务逻辑检查的标记
	isDrive := false

	switch {
	case strings.HasSuffix(topic, "/state"):
		c.mu.Lock()
		if c.lastState != payload {
			c.lastState = payload
			isDrive = true
		}
		c.mu.Unlock()

	case strings.HasSuffix(topic, "/shift_state"):
		c.mu.Lock()
		prev := c.lastShiftState
		if prev != payload {
			c.lastShiftState = payload
			isDrive = true
		}
		// 跟踪进入 P 档的时间：从非 P 切入 P，或首次收到 P（ MQTT retained ），重新开始计时
		if payload == "P" {
			if prev != "P" || c.pGearSince.IsZero() {
				c.pGearSince = time.Now()
			}
		} else {
			c.pGearSince = time.Time{}
			// 离开 P 档（车辆开走）：取消未锁复核计时器
			if c.unlockCheckTimer != nil {
				c.unlockCheckTimer.Stop()
				c.unlockCheckTimer = nil
			}
		}
		// 车辆启动判定：P 档 -> D 档后，启动确认计时器
		if prev == "P" && payload == "D" {
			c.scheduleStartConfirmLocked()
		} else if payload != "D" && c.startConfirmTimer != nil {
			// 尚未确认期间档位离开 D，取消本次启动判定
			c.startConfirmTimer.Stop()
			c.startConfirmTimer = nil
			log.Printf("[启动确认] 档位已离开 D（当前 %s），取消启动通知", payload)
		}
		c.mu.Unlock()

	case strings.HasSuffix(topic, "/charging_state"):
		c.mu.Lock()
		isCharge := c.lastChargingState != payload
		c.lastChargingState = payload
		state := payload
		c.mu.Unlock()
		if isCharge {
			go c.checkChargingCondition(state)
		}

	case strings.HasSuffix(topic, "/is_user_present"):
		c.mu.Lock()
		val := payload == "true"
		if c.lastUserPresent != val {
			c.lastUserPresent = val
			isDrive = true
		}
		c.mu.Unlock()

	case strings.HasSuffix(topic, "/driver_front_door_open"):
		c.mu.Lock()
		val := payload == "true"
		opened := false
		if c.lastDriverDoorOpen != val {
			// 从关闭变为打开，视为一次开门事件
			opened = !c.lastDriverDoorOpen && val
			c.lastDriverDoorOpen = val
			isDrive = true
		}
		c.mu.Unlock()
		if opened {
			go c.processDoorOpen()
		}

	case strings.HasSuffix(topic, "/locked"):
		c.mu.Lock()
		val := payload == "true"
		if c.lastLocked != val {
			c.lastLocked = val
			isDrive = true
		}
		if val {
			// 已上锁：取消未锁复核计时器
			if c.unlockCheckTimer != nil {
				c.unlockCheckTimer.Stop()
				c.unlockCheckTimer = nil
				log.Println("[锁车检查] 车辆已上锁，取消未锁复核")
			}
		} else {
			// 未锁：安排延时复核，到时仍为长时间 P 档且未锁才提醒
			c.scheduleUnlockCheckLocked()
		}
		c.mu.Unlock()

	// 以下是非关键状态，只更新内存值，不触发 scheduleStateSettle
	case strings.HasSuffix(topic, "/battery_level"):
		if val, err := strconv.ParseFloat(payload, 64); err == nil {
			c.mu.Lock()
			c.lastBatteryLevel = val
			c.mu.Unlock()
		}
	case strings.HasSuffix(topic, "/ideal_battery_range_km"):
		if val, err := strconv.ParseFloat(payload, 64); err == nil {
			c.mu.Lock()
			c.lastIdealRangeKM = val
			c.mu.Unlock()
		}
	case strings.HasSuffix(topic, "/charge_limit_soc"):
		if val, err := strconv.Atoi(payload); err == nil {
			c.mu.Lock()
			c.chargeLimitSoc = val
			c.mu.Unlock()
		}
	}

	// 只有关键状态变化，才启动/重置沉淀计时器
	if isDrive {
		c.scheduleStateSettle()
	}
}

// scheduleStateSettle 负责状态沉淀
func (c *Client) scheduleStateSettle() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stateSettleTimer != nil {
		c.stateSettleTimer.Stop()
	}

	c.stateSettleTimer = time.AfterFunc(1*time.Second, func() {
		// 检查行程是否结束
		c.checkTripEndCondition()
	})
}

// scheduleStartConfirmLocked 安排一次启动确认：调用者需持有 c.mu。
func (c *Client) scheduleStartConfirmLocked() {
	sec := c.cfg.StartConfirmSeconds
	if sec <= 0 {
		sec = 10
	}
	if c.startConfirmTimer != nil {
		c.startConfirmTimer.Stop()
	}
	log.Printf("[启动确认] 检测到 P→D，%d 秒后复核档位...", sec)
	c.startConfirmTimer = time.AfterFunc(time.Duration(sec)*time.Second, c.confirmVehicleStart)
}

// confirmVehicleStart 在 P→D 后延时触发，若此刻仍为 D 档则推送启动通知。
func (c *Client) confirmVehicleStart() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[启动确认] 崩溃: %v\n%s", r, string(debug.Stack()))
		}
	}()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.startConfirmTimer = nil
	if c.lastShiftState != "D" {
		log.Printf("[启动确认] 复核时档位为 %s（非 D），取消启动通知", c.lastShiftState)
		return
	}
	if !c.cfg.StartNotifyEnabled {
		log.Printf("[启动确认] 启动通知已关闭，跳过")
		return
	}

	now := time.Now()
	if !c.lastStartNotifyTime.IsZero() &&
		now.Sub(c.lastStartNotifyTime) < time.Duration(c.cfg.StartNotifyCooldownSec)*time.Second {
		log.Printf("[启动通知] 冷却期内跳过推送 (距上次 %.0fs)",
			now.Sub(c.lastStartNotifyTime).Seconds())
		return
	}
	c.lastStartNotifyTime = now

	content := fmt.Sprintf(`时间: %s
当前电量: %.0f%%
当前表显: %.1f km`,
		now.Local().Format("15:04"),
		c.lastBatteryLevel,
		c.lastIdealRangeKM,
	)
	title := fmt.Sprintf("🚗 %s 车辆已启动 🔑", c.carName)

	log.Printf("🚀 [启动确认] 持续 D 档已达 %d 秒，推送启动通知", c.cfg.StartConfirmSeconds)

	go func() {
		if err := notifier.SendNotification(c.cfg.APIToken, title, content); err != nil {
			log.Printf("❌ 启动通知推送失败: %v", err)
		} else {
			log.Println("✅ 启动通知已推送")
		}
	}()
}

// scheduleUnlockCheckLocked 安排一次未锁状态复核。调用者需持有 c.mu。
func (c *Client) scheduleUnlockCheckLocked() {
	if !c.cfg.UnlockNotifyEnabled {
		return
	}
	delay := c.cfg.UnlockNotifyDelaySec
	if delay <= 0 {
		delay = 120
	}
	if c.unlockCheckTimer != nil {
		c.unlockCheckTimer.Stop()
	}
	log.Printf("[锁车检查] 检测到车辆未锁，%d 秒后复核...", delay)
	c.unlockCheckTimer = time.AfterFunc(time.Duration(delay)*time.Second, c.confirmUnlockReminder)
}

// confirmUnlockReminder 延时复核：长时间 P 档停放且仍未锁才推送忘记锁车提醒。
func (c *Client) confirmUnlockReminder() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[锁车检查] 崩溃: %v\n%s", r, string(debug.Stack()))
		}
	}()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.unlockCheckTimer = nil

	if !c.cfg.UnlockNotifyEnabled {
		return
	}
	if c.lastLocked {
		log.Println("[锁车检查] 复核时车辆已上锁，跳过")
		return
	}

	// 必须是长时间 P 档停放：刚停车时人还在下车，不打扰
	pDuration := time.Duration(0)
	if !c.pGearSince.IsZero() {
		pDuration = time.Since(c.pGearSince)
	}
	if c.lastShiftState != "P" {
		log.Printf("[锁车检查] 复核时档位为 %s（非 P），跳过", c.lastShiftState)
		return
	}
	if pDuration < time.Duration(c.cfg.DoorNotifyPDurationSec)*time.Second {
		// 还在长时间 P 档的等待期内：未锁状态还在持续，过一段时间再复核
		log.Printf("[锁车检查] P 档仅持续 %.0fs（需 %ds），未锁仍在持续，稍后复核",
			pDuration.Seconds(), c.cfg.DoorNotifyPDurationSec)
		c.scheduleUnlockCheckLocked()
		return
	}

	now := time.Now()
	if !c.lastUnlockNotifyTime.IsZero() &&
		now.Sub(c.lastUnlockNotifyTime) < time.Duration(c.cfg.UnlockNotifyCooldownSec)*time.Second {
		log.Printf("[锁车通知] 冷却期内跳过推送 (距上次 %.0fs)",
			now.Sub(c.lastUnlockNotifyTime).Seconds())
		return
	}
	c.lastUnlockNotifyTime = now

	content := fmt.Sprintf(`时间: %s
已停放: %.0f 分钟
当前电量: %.0f%%
当前表显: %.1f km
提醒: 车辆长时间停放且未上锁，请确认是否忘记锁车`,
		now.Local().Format("15:04"),
		pDuration.Minutes(),
		c.lastBatteryLevel,
		c.lastIdealRangeKM,
	)
	title := fmt.Sprintf("🔓 %s 可能忘记锁车 🔓", c.carName)

	log.Printf("🔓 [锁车检查] 长时间 P 档未锁，推送提醒")

	go func() {
		if err := notifier.SendNotification(c.cfg.APIToken, title, content); err != nil {
			log.Printf("❌ 忘锁通知推送失败: %v", err)
		} else {
			log.Println("✅ 忘锁通知已推送")
		}
	}()
}

// processDoorOpen 发送车门打开通知，包含冷却控制。
func (c *Client) processDoorOpen() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[开门检查] 崩溃: %v\n%s", r, string(debug.Stack()))
		}
	}()

	if !c.cfg.DoorNotifyEnabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	// 仅"长时间 P 档后开门"才通知：刚停车（P 档未达阈值）或行驶中开门都不推送
	pDuration := time.Duration(0)
	if !c.pGearSince.IsZero() {
		pDuration = now.Sub(c.pGearSince)
	}
	if c.lastShiftState != "P" || pDuration < time.Duration(c.cfg.DoorNotifyPDurationSec)*time.Second {
		log.Printf("[开门通知] 非长时间 P 档（档位 %s，P 档持续 %.0fs < %ds），跳过推送",
			c.lastShiftState, pDuration.Seconds(), c.cfg.DoorNotifyPDurationSec)
		return
	}

	if !c.lastDoorNotifyTime.IsZero() &&
		now.Sub(c.lastDoorNotifyTime) < time.Duration(c.cfg.DoorNotifyCooldownSec)*time.Second {
		log.Printf("[开门通知] 冷却期内跳过推送 (距上次 %.0fs)",
			now.Sub(c.lastDoorNotifyTime).Seconds())
		return
	}
	c.lastDoorNotifyTime = now

	content := fmt.Sprintf(`时间: %s
当前电量: %.0f%%
当前表显: %.1f km`,
		now.Local().Format("15:04"),
		c.lastBatteryLevel,
		c.lastIdealRangeKM,
	)
	title := fmt.Sprintf("🚗 %s 车门已打开 🚪", c.carName)

	log.Printf("🚪 [开门检查] 检测到车门打开，准备推送通知")

	go func() {
		if err := notifier.SendNotification(c.cfg.APIToken, title, content); err != nil {
			log.Printf("❌ 开门通知推送失败: %v", err)
		} else {
			log.Println("✅ 开门通知已推送")
		}
	}()
}

// checkChargingCondition 接收触发时的状态快照，避免跨 goroutine 读到被并发修改的字段。
func (c *Client) checkChargingCondition(state string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[充电检查] 崩溃: %v\n%s", r, string(debug.Stack()))
		}
	}()
	switch state {
	case "Starting":
		c.processChargeStart()
	case "Complete":
		// 充满电单独推送"已充满"通知；关闭开关时回退到原"充电完成"通知
		c.processChargeFinish(true)
	case "Stopped", "Disconnected":
		c.processChargeFinish(false)
	}
}

func (c *Client) checkTripEndCondition() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 诊断日志：查看当前内存中的状态快照
	log.Printf("[行程检查] 当前状态: State=%s, Shift=%s, UserPresent=%v, Locked=%v",
		c.lastState, c.lastShiftState, c.lastUserPresent, c.lastLocked)

	isNotDriving := c.lastState == "online"
	isParked := c.lastShiftState == "P" || !c.lastUserPresent || c.lastDriverDoorOpen || !c.lastLocked

	if isNotDriving || isParked {
		if c.debounceTimer == nil {
			log.Printf("[行程检查] 满足结束条件，启动 %d 秒防抖计时...", c.cfg.PushDebounceSec)
			c.debounceTimer = time.AfterFunc(
				time.Duration(c.cfg.PushDebounceSec)*time.Second,
				c.processTripEnd,
			)
		}
	} else {
		if c.debounceTimer != nil {
			log.Println("[行程检查] 车辆重新开始行驶，取消防抖计时")
			c.debounceTimer.Stop()
			c.debounceTimer = nil
		}
	}
}

// 充电开始通知 - 加入了限值显示
func (c *Client) processChargeStart() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().Local().Format("15:04")
	limitStr := ""
	if c.chargeLimitSoc > 0 {
		limitStr = fmt.Sprintf(" (限值: %d%%)", c.chargeLimitSoc)
	}

	content := fmt.Sprintf(`时间: %s
当前电量: %.0f%%%s
当前表显: %.1f km`,
		now, c.lastBatteryLevel, limitStr, c.lastIdealRangeKM,
	)

	title := fmt.Sprintf("🚗 %s 充电开始 🔌", c.carName)

	go func() {
		if err := notifier.SendNotification(c.cfg.APIToken, title, content); err != nil {
			log.Printf("❌ 充电开始推送失败: %v", err)
		} else {
			log.Println("✅ 充电开始通知已推送")
		}
	}()
}

// processChargeFinish 处理充电结束流程。isComplete=true 表示真正充满（Complete 状态）。
func (c *Client) processChargeFinish(isComplete bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	charge, err := db.GetLatestCharge(c.cfg.CarID)
	if err != nil {
		log.Printf("❌ [充电检查] 数据库查询失败: %v", err)
		return
	}

	if charge.ID == c.lastChargeID || charge.ID == 0 || charge.EndDate.IsZero() {
		log.Printf("⏳ [充电检查] 数据未就绪: 当前数据库ID=%d, 上次ID=%d, 结束时间=%v。准备进入重试...",
			charge.ID, c.lastChargeID, charge.EndDate)

		c.tryWithBackoff(&c.retryCountCharge, 3, c.cfg.PushDebounceSec, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			ch, e := db.GetLatestCharge(c.cfg.CarID)
			if e != nil {
				log.Printf("❌ [充电重试] 查询失败: %v", e)
				return false
			}

			if ch.ID != c.lastChargeID && ch.ID != 0 && !ch.EndDate.IsZero() {
				log.Printf("✅ [充电重试] 成功捕获新数据! ID: %d", ch.ID)
				c.lastChargeID = ch.ID
				c.doChargeNotification(ch, isComplete)
				return true
			}

			log.Printf("😴 [充电重试] 数据仍未更新，继续等待... (当前DB ID: %d)", ch.ID)
			return false
		}, "充电通知")
		return
	}

	// 如果直接查到了新 ID
	log.Printf("🚀 [充电检查] 直接捕获新数据: ID %d", charge.ID)
	c.lastChargeID = charge.ID
	c.doChargeNotification(charge, isComplete)
}

// 充电结束通知。isComplete=true 表示充满，推送专用"已充满"文案；调用方需持有 c.mu。
func (c *Client) doChargeNotification(charge *models.Charge, isComplete bool) {
	chargeType := c.getChargeType(charge)

	content := fmt.Sprintf(`时间: %s→%s | 历时: %s
充入: %.1f kWh | 类型: %s
表显: %.0f→%.0f km (+%.1f km)
电量: %.0f→%.0f%% (+%.0f%%)`,
		charge.StartDate.Local().Format("15:04"), charge.EndDate.Local().Format("15:04"), formatDuration(charge.DurationMin),
		charge.ChargeEnergyAdded, chargeType,
		charge.StartIdealRangeKM, charge.EndIdealRangeKM, charge.EndIdealRangeKM-charge.StartIdealRangeKM,
		float64(charge.StartBatteryLevel), float64(charge.EndBatteryLevel), float64(charge.EndBatteryLevel-charge.StartBatteryLevel),
	)

	var title, logTag string
	if isComplete && c.cfg.FullChargeNotifyEnabled {
		title = fmt.Sprintf("🔋 %s 已充满 ✅", c.carName)
		logTag = "充满电通知"
	} else {
		title = fmt.Sprintf("🚗 %s 充电%s 🔋", c.carName, getChargeState(c.lastChargingState))
		logTag = "充电通知"
	}

	go func() {
		if err := notifier.SendNotification(c.cfg.APIToken, title, content); err != nil {
			log.Printf("❌ %s推送失败: %v", logTag, err)
		} else {
			log.Printf("✅ %s已推送 (ID: %d)", logTag, charge.ID)
		}
	}()
}

func (c *Client) processTripEnd() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 定时器已触发，复位以便下一行程能正常创建防抖计时
	if c.debounceTimer != nil {
		c.debounceTimer = nil
	}

	result, err := db.GetLatestDrive(c.cfg.CarID)
	if err != nil {
		log.Printf("❌ [行程检查] 数据库查询失败: %v", err)
		return
	}

	drive := &result.Drive
	log.Printf("[行程检查] 数据库最新 ID: %d, 上次推送 ID: %d, 结束时间: %v",
		drive.ID, c.lastDriveID, drive.EndDate)

	if drive.ID == c.lastDriveID || drive.ID == 0 || drive.EndDate.IsZero() {
		log.Printf("⏳ [行程检查] 数据未就绪（可能数据库尚未更新），进入重试模式...")

		c.tryWithBackoff(&c.retryCountDrive, 3, c.cfg.PushDebounceSec, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			res, e := db.GetLatestDrive(c.cfg.CarID)
			if e != nil {
				log.Printf("❌ [行程重试] 查询失败: %v", e)
				return false
			}

			// 重试成功的标准：ID 变了 且 已经有结束时间
			if res.Drive.ID != c.lastDriveID && res.Drive.ID != 0 && !res.Drive.EndDate.IsZero() {
				// 校验是否是无效短行程
				if res.Drive.Distance == 0 || res.Drive.DurationMin == 0 {
					log.Printf("⏭️ [行程重试] 捕获到新行程 ID %d，但属于无效短行程，跳过推送", res.Drive.ID)
					c.lastDriveID = res.Drive.ID // 标记已处理
					return true
				}

				log.Printf("✅ [行程重试] 成功捕获新行程数据! ID: %d", res.Drive.ID)
				c.lastDriveID = res.Drive.ID
				c.doTripNotification(res)
				return true
			}

			log.Printf("😴 [行程重试] 数据库 ID 仍为 %d，继续等待更新...", res.Drive.ID)
			return false
		}, "行程通知")
		return
	}

	// 如果运气好，第一次查就查到了新 ID
	if drive.Distance == 0 || drive.DurationMin == 0 {
		log.Printf("⏭️ [行程检查] 忽略无效短行程 (ID: %d)", drive.ID)
		c.lastDriveID = drive.ID
		return
	}

	log.Printf("🚀 [行程检查] 直接捕获到新行程: ID %d", drive.ID)
	c.lastDriveID = drive.ID
	c.doTripNotification(result)
}

// tryWithBackoff 指数退避。retryMu 保护 retryCount 的并发访问。
func (c *Client) tryWithBackoff(retryCount *int, maxRetries int, baseDelaySec int, action func() bool, logPrefix string) {
	c.tryWithBackoffLocked(retryCount, maxRetries, baseDelaySec, action, logPrefix)
}

func (c *Client) tryWithBackoffLocked(retryCount *int, maxRetries int, baseDelaySec int, action func() bool, logPrefix string) {
	c.retryMu.Lock()
	*retryCount++
	if *retryCount > maxRetries {
		log.Printf("⏹️ %s 重试超过 %d 次，放弃", logPrefix, maxRetries)
		*retryCount = 0
		c.retryMu.Unlock()
		return
	}
	cur := *retryCount
	c.retryMu.Unlock()

	delay := time.Duration(baseDelaySec*(1<<uint(cur-1))) * time.Second
	log.Printf("🔄 %s 第 %d 次尝试，等待 %.0f 秒...", logPrefix, cur, delay.Seconds())

	time.AfterFunc(delay, func() {
		ok := action()
		c.retryMu.Lock()
		shouldRetry := !ok && *retryCount < maxRetries
		if !shouldRetry {
			*retryCount = 0
		}
		c.retryMu.Unlock()
		// 必须在释放 retryMu 之后再递归，sync.Mutex 不可重入
		if shouldRetry {
			c.tryWithBackoffLocked(retryCount, maxRetries, baseDelaySec, action, logPrefix)
		}
	})
}

// 行程通知内容。调用方需持有 c.mu。
func (c *Client) doTripNotification(result *db.DriveWithSOC) {
	drive := &result.Drive
	socUsed := result.StartSOC - result.EndSOC
	rangeReduced := drive.StartIdealRangeKM - drive.EndIdealRangeKM
	achieveRate := 0.0
	if rangeReduced > 0 {
		achieveRate = (drive.Distance / rangeReduced) * 100
	}
	avgSpeed := calculateAvgSpeed(drive.Distance, drive.DurationMin)

	content := fmt.Sprintf(`时间: %s→%s | 历时: %s
距离: %.1f km | 均速: %.1f km/h
表显: %.0f→%.0f km (-%.1f km)
电量: %.0f→%.0f%% (-%.0f%%) 达成率: %.1f%%`,
		drive.StartDate.Local().Format("15:04"), drive.EndDate.Local().Format("15:04"), formatDuration(drive.DurationMin),
		drive.Distance, avgSpeed,
		drive.StartIdealRangeKM, drive.EndIdealRangeKM, rangeReduced,
		result.StartSOC, result.EndSOC, socUsed, achieveRate,
	)

	title := fmt.Sprintf("🚗 %s 行程通知 📍", c.carName)

	go func() {
		if err := notifier.SendNotification(c.cfg.APIToken, title, content); err != nil {
			log.Printf("❌ 行程通知推送失败: %v", err)
		} else {
			log.Printf("✅ 行程通知已推送 (ID: %d)", drive.ID)
		}
	}()
}

// 优化后的快充判断逻辑
func (c *Client) getChargeType(charge *models.Charge) string {
	// 1. 检查 FastChargerPresent 标志位
	if charge.FastChargerPresent != nil && *charge.FastChargerPresent {
		return "快充 (DC)"
	}
	// 2. 检查相位，DC 充电通常显示为 0 或 null
	if charge.ChargerPhases != nil && *charge.ChargerPhases == 0 {
		return "快充 (DC)"
	}
	// 3. 功率兜底判断 (大于 30kW 基本为直流桩)
	if charge.ChargerPower > 30 {
		return "快充 (DC)"
	}
	return "慢充 (AC)"
}

func getChargeState(state string) string {
	if state == "Complete" {
		return "完成"
	}
	if state == "Disconnected" {
		return "断开"
	}
	if state == "Stopped" {
		return "停止"
	}
	return "结束"
}

func formatDuration(minutes int16) string {
	if minutes < 60 {
		return fmt.Sprintf("%dmin", minutes)
	}
	hours := minutes / 60
	mins := minutes % 60
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dmin", hours, mins)
}

func calculateAvgSpeed(distanceKm float64, durationMin int16) float64 {
	if durationMin == 0 {
		return 0
	}
	return distanceKm * 60 / float64(durationMin)
}
