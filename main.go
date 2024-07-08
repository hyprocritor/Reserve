package main

import (
	"errors"
	"github.com/imroc/req/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var Cookie string

var UA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5_1 like Mac OS X) AppleWebKit/618.2.12.10.9 (KHTML, like Gecko) Mobile/21F90 BiliApp/80300100 os/ios model/iPhone 14 Pro Max mobi_app/iphone build/80300100 osVer/17.5.1 network/2 channel/AppStore Buvid/${BUVID} c_locale/zh-Hans_CN s_locale/zh-Hans_JP sessionID/11fa54f6 disable_rcmd/0"

var InfoUrl = "https://api.bilibili.com/x/activity/bws/online/park/reserve/info?csrf=${csrf}&reserve_date=20240712,20240713,20240714"

const DoUrl = "https://api.bilibili.com/x/activity/bws/online/park/reserve/do"

var Client = req.C().SetUserAgent(UA).SetTLSFingerprintIOS().ImpersonateSafari()
var logger *zap.Logger
var nameMap map[int]string
var currentTimeOffset time.Duration
var TicketData = map[string]InfoTicketInfo{}
var ch = make(chan *req.Response)

// TargetPair Reserve ID: ticket ID
var TargetPair = map[int]string{}

func InitLogger() {
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	l, _ := config.Build()
	logger = l
}

func GetReservationInfo() (*InfoResponse, error) {
	var result InfoResponse
	_, err := Client.R().
		SetHeader("Cookie", Cookie).
		SetSuccessResult(&result).Get(InfoUrl)
	if err != nil {
		logger.Error("获取Info接口错误", zap.Error(err))
		return nil, err
	}
	if result.Code != 0 {
		logger.Error("Info 返回不为0", zap.String("message", result.Message))
		return nil, err
	}

	return &result, nil
}

func GetUserTicketInfo(info *InfoData) {
	for _, ticket := range info.UserTicketInfo {
		logger.Info("当前可用票", zap.String("票别", ticket.SkuName), zap.String("票号", ticket.Ticket), zap.String("日期", ticket.ScreenName))
		TicketData[ticket.Ticket] = ticket
	}
}

func writeAllResponseToFile() {
	//init a writer
	f, err := os.Create("response.txt")
	if err != nil {
		logger.Error("读写文件错误", zap.Error(err))
	}
	defer f.Close()
	for {
		select {
		case r := <-ch:
			_, err := f.WriteString(r.String() + "\n")
			if err != nil {
				logger.Error("写文件错误", zap.Error(err))
			}
		}
	}

}

func CallReserve(csrf string, reserveId int, ticketNo string) (*DoResponse, error) {
	var result DoResponse
	body := "csrf=" + csrf +
		"&inter_reserve_id=" + strconv.Itoa(reserveId) +
		"&ticket_no=" + ticketNo
	resp, err := Client.R().
		SetHeader("cookie", Cookie).
		SetHeader("content-type", "application/x-www-form-urlencoded").
		SetHeader("referer", "https://www.bilibili.com/blackboard/bw/2024/bws_event.html?navhide=1&stahide=1&native.theme=2&night=1#/Order/FieldOrder").
		SetSuccessResult(&result).
		SetBody(body).Post(DoUrl)
	ch <- resp
	if err != nil {
		if resp != nil && resp.StatusCode == 429 {
			logger.Error("429 - 请求频率过高", zap.Error(err))
			return nil, err
		}
		if resp != nil && resp.StatusCode == 412 {
			logger.Error("412 - STATUS CODE", zap.Error(err))
			return nil, err
		}
		logger.Error("获取Do接口错误", zap.Error(err))
		return nil, err
	}

	return &result, nil
}

func GetCSRFFromCookie(cookie string) string {
	//Split the cookie
	cookieArray := strings.Split(cookie, ";")
	for _, c := range cookieArray {
		if strings.Contains(c, "bili_jct") {
			return strings.Split(c, "=")[1]
		}
	}
	logger.Error("未找到CSRF Token")
	return ""
}

func getReservationStartDate(info InfoData, reserveId int) (int64, error) {
	for _, value := range info.ReserveList {
		for _, v := range value {
			if v.ReserveID == reserveId {
				return v.ReserveBeginTime, nil
			}
		}
	}
	return -1, errors.New("未找到预约信息")
}

func createReservationJob(reserveId int, ticketNo string, csrfToken string, info InfoData, wg *sync.WaitGroup) {
	reservationStartDate, err := getReservationStartDate(info, reserveId)
	if err != nil {
		logger.Error("无法获取预约开始时间", zap.Error(err))
	}

	go doReserve(reservationStartDate, reserveId, ticketNo, csrfToken, wg)

}

func doReserve(startTime int64, reservedId int, ticketId string, csrfToken string, wg *sync.WaitGroup) {
	defer wg.Done()
	//calculate the timer decay
	realStartTime := startTime * 1000
	ticket := TicketData[ticketId]
	for {
		//get start time
		currentTime := time.Now().Add(currentTimeOffset).UnixMilli()
		timeDifference := realStartTime - currentTime
		if timeDifference > 0 {
			// wait for half of the difference
			waitFor := timeDifference / 2
			logger.Info(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 等待预约开始", zap.Time("开始时间", time.Unix(startTime, 0)), zap.Time("当前时间", time.UnixMilli(currentTime)), zap.Duration("时间偏移", currentTimeOffset), zap.Any("等待", time.Duration(timeDifference)*time.Millisecond/2))
			time.Sleep(time.Duration(waitFor) * time.Millisecond)
			continue
		}
		//do reserve
		reservation, err := CallReserve(csrfToken, reservedId, ticketId)
		if err != nil {
			logger.Error(nameMap[reservedId]+" @"+ticket.ScreenName+" - 预约失败，内部错误，重试中。", zap.Error(err))
			continue
		}
		if reservation.Code != 0 {
			switch reservation.Code {
			case 412:
			case 429:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 412 / 429 重试中", zap.String("message", reservation.Message))
				time.Sleep(300 * time.Millisecond)
				continue
			case 76650:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 操作频繁，等待重试。", zap.String("message", reservation.Message))
				time.Sleep(300 * time.Millisecond)
				continue
			case 76647:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 该账户预约次数达到上限，退出此任务。", zap.String("message", reservation.Message))
				return
			case -702:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 请求频率过高，等待重试。", zap.String("message", reservation.Message))
				time.Sleep(500 * time.Millisecond)
				continue
			case 75574:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 本项目预约已满。准备回流。30秒检测一次。", zap.String("message", reservation.Message))
				time.Sleep(30 * time.Second)
				continue
			case 75637:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 项目可能未开始！紧急重试！", zap.String("message", reservation.Message))
				return
			default:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 警告：未知返回代码！防止风控，立即结束当前任务！", zap.String("message", reservation.Message), zap.Any("code", reservation.Code))
				return
			}
		}
		logger.Info(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 预约成功", zap.String("message", reservation.Message))
		return
	}
}

func createReservationIDandNameMap(info InfoData) {
	result := make(map[int]string)
	for _, value := range info.ReserveList {
		for _, v := range value {
			result[v.ReserveID] = v.ActTitle
		}
	}
	nameMap = result
}

func syncTimeOffset() {
	timeOffset, err := GetNTPOffset()
	if err != nil {
		logger.Error("获取时间失败", zap.Error(err))
	}
	if timeOffset != nil {
		logger.Info("当前时间偏移", zap.Duration("时间偏移", *timeOffset))
		currentTimeOffset = *timeOffset
	} else {
		logger.Warn("未获取到时间偏移")
	}

}
func main() {
	InitLogger()
	logger.Info("程序已启动。")
	var configFile string
	//load args
	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}
	if configFile == "" {
		configFile = "config.json"
	}
	config, err := LoadConfig(configFile)
	if err != nil {
		logger.Error("无法加载配置文件", zap.Error(err))
		return
	}
	logger.Info("配置文件已加载", zap.String("文件", configFile))
	Cookie = config.Cookie
	csrfToken := GetCSRFFromCookie(Cookie)
	if csrfToken == "" {
		logger.Error("获取CSRF Token失败")
		return
	}
	logger.Info("CSRF Token", zap.String("token", csrfToken))
	//set buvid in ua
	UA = strings.ReplaceAll(UA, "${BUVID}", config.BuvID)
	//set csrf token
	InfoUrl = strings.ReplaceAll(InfoUrl, "${csrf}", csrfToken)

	logger.Info("用户 UA", zap.String("UA", UA))
	TargetPair = convertJobKeyType(config.Job)
	timeOffset, err := GetNTPOffset()
	if err != nil {
		logger.Error("获取时间失败", zap.Error(err))
	}
	if timeOffset != nil {
		logger.Info("当前时间偏移", zap.Duration("时间偏移", *timeOffset))
		currentTimeOffset = *timeOffset
	} else {
		logger.Warn("未获取到时间偏移")
	}

	// 获取预约信息
	info, err := GetReservationInfo()
	if err != nil {
		logger.Error("获取预约信息失败", zap.Error(err))
		return
	}
	go writeAllResponseToFile()
	// 获取用户可用票
	GetUserTicketInfo(&info.Data)
	createReservationIDandNameMap(info.Data)

	var wg sync.WaitGroup
	//set up time sync
	syncTimeOffset()
	//测试预约
	//resp, err := CallReserve(csrfToken, 6016, "15111332527932")
	//if err != nil {
	//	println(err)
	//	return
	//}
	//print resp code and message
	//logger.Info("预约结果", zap.Int("code", resp.Code), zap.String("message", resp.Message))
	// 预约

	for reserveId, ticketNo := range TargetPair {
		wg.Add(1)
		createReservationJob(reserveId, ticketNo, csrfToken, info.Data, &wg)
	}
	wg.Wait()
	logger.Info("所有任务已完成。")
}
