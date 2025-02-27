// SPDX-FileCopyrightText: 2018 - 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later

package xsettings

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	dbus "github.com/godbus/dbus/v5"
	"github.com/linuxdeepin/dde-api/userenv"
	gio "github.com/linuxdeepin/go-gir/gio-2.0"
	"github.com/linuxdeepin/go-lib/keyfile"
	"github.com/linuxdeepin/go-lib/xdg/basedir"
	x "github.com/linuxdeepin/go-x11-client"
	"github.com/linuxdeepin/go-x11-client/ext/randr"
)

const (
	EnvDeepinWineScale      = "DEEPIN_WINE_SCALE"
	gsKeyScaleFactor        = "scale-factor"
	gsKeyWindowScale        = "window-scale"
	gsKeyGtkCursorThemeSize = "gtk-cursor-theme-size"
	gsKeyIndividualScaling  = "individual-scaling"
	baseCursorSize          = 24

	qtThemeSection               = "Theme"
	qtThemeKeyScreenScaleFactors = "ScreenScaleFactors"
	qtThemeKeyScaleFactor        = "ScaleFactor"
	qtThemeKeyScaleLogicalDpi    = "ScaleLogicalDpi"
)

// 设置单个缩放值的关键方法
func (m *XSManager) setScaleFactor(scale float64, emitSignal bool) {
	logger.Debug("setScaleFactor", scale)
	m.gs.SetDouble(gsKeyScaleFactor, scale)

	// if 1.7 < scale < 2, window scale = 2
	windowScale := int32(math.Trunc((scale+0.3)*10) / 10)
	if windowScale < 1 {
		windowScale = 1
	}
	oldWindowScale := m.gs.GetInt(gsKeyWindowScale)
	if oldWindowScale != windowScale {
		m.gs.SetInt(gsKeyWindowScale, windowScale)
	}

	cursorSize := int32(baseCursorSize * scale)
	m.gs.SetInt(gsKeyGtkCursorThemeSize, cursorSize)
	// set cursor size for deepin-metacity
	gsWrapGDI := gio.NewSettings("com.deepin.wrap.gnome.desktop.interface")
	gsWrapGDI.SetInt("cursor-size", cursorSize)
	gsWrapGDI.Unref()

	m.setScaleFactorForPlymouth(int(windowScale), emitSignal)
}

func parseScreenFactors(str string) map[string]float64 {
	pairs := strings.Split(str, ";")
	result := make(map[string]float64)
	for _, value := range pairs {
		kv := strings.SplitN(value, "=", 2)
		if len(kv) != 2 {
			continue
		}

		value, err := strconv.ParseFloat(kv[1], 64)
		if err != nil {
			logger.Warning(err)
			continue
		}

		result[kv[0]] = value
	}

	return result
}

func joinScreenScaleFactors(v map[string]float64) string {
	pairs := make([]string, len(v))
	idx := 0
	for key, value := range v {
		pairs[idx] = fmt.Sprintf("%s=%.2f", key, value)
		idx++
	}
	return strings.Join(pairs, ";")
}

func getQtThemeFile() string {
	return filepath.Join(basedir.GetUserConfigDir(), "deepin/qt-theme.ini")
}

func cleanUpDdeEnv() error {
	ue, err := userenv.Load()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	needSave := false
	for _, key := range []string{
		"QT_SCALE_FACTOR",
		"QT_SCREEN_SCALE_FACTORS",
		"QT_AUTO_SCREEN_SCALE_FACTOR",
		"QT_FONT_DPI",
		EnvDeepinWineScale,
	} {
		if _, ok := ue[key]; ok {
			delete(ue, key)
			needSave = true
		}
	}

	if needSave {
		err = userenv.Save(ue)
	}
	return err
}

func (m *XSManager) setScreenScaleFactorsForQt(factors map[string]float64) error {
	filename := getQtThemeFile()
	kf := keyfile.NewKeyFile()
	err := kf.LoadFromFile(filename)
	if err != nil && !os.IsNotExist(err) {
		logger.Warning("failed to load qt-theme.ini:", err)
	}

	var value string
	switch len(factors) {
	case 0:
		return errors.New("factors is empty")
	case 1:
		value = strconv.FormatFloat(getMapFirstValueSF(factors), 'f', 2, 64)
	default:
		value = joinScreenScaleFactors(factors)
		value = strconv.Quote(value)
	}
	kf.SetValue(qtThemeSection, qtThemeKeyScreenScaleFactors, value)
	kf.DeleteKey(qtThemeSection, qtThemeKeyScaleFactor)
	kf.SetValue(qtThemeSection, qtThemeKeyScaleLogicalDpi, "-1,-1")

	err = os.MkdirAll(filepath.Dir(filename), 0755)
	if err != nil {
		return err
	}

	err = kf.SaveToFile(filename)
	if err != nil {
		return err
	}

	err = m.updateGreeterQtTheme(kf)
	return err
}

func getMapFirstValueSF(m map[string]float64) float64 {
	for _, value := range m {
		return value
	}
	return 0
}

func getPrimaryScreenName(xConn *x.Conn) (string, error) {
	rootWin := xConn.GetDefaultScreen().Root
	getPrimaryReply, err := randr.GetOutputPrimary(xConn, rootWin).Reply(xConn)
	if err != nil {
		logger.Debug("Failed to get output primary:", err)
		return getPrimaryScreenFromBus()
	}
	outputInfo, err := randr.GetOutputInfo(xConn, getPrimaryReply.Output,
		x.CurrentTime).Reply(xConn)
	if err != nil {
		logger.Debug("Failed to get output info:", err)
		return getPrimaryScreenFromBus()
	}
	return outputInfo.Name, nil
}

var (
	_sessionConn *dbus.Conn
)

func getPrimaryScreenFromBus() (string, error) {
	if _sessionConn == nil {
		conn, err := dbus.SessionBus()
		if err != nil {
			return "", err
		}
		_sessionConn = conn
	}

	variant, err := _sessionConn.Object("org.deepin.dde.Display1",
		"/org/deepin/dde/Display1").GetProperty("org.deepin.dde.Display1.Primary")
	if err != nil {
		return "", err
	}

	primary, ok := (variant.Value()).(string)
	if !ok {
		return "", fmt.Errorf("invalid primary signature: %s", variant.String())
	}
	return primary, nil
}

// 不发送通知版本, 设置流程会转到 setScreenScaleFactors
func (m *XSManager) setScaleFactorWithoutNotify(scale float64) error {
	err := m.setScreenScaleFactors(singleToMapSF(scale), false)
	return err
}

func getSingleScaleFactor(factors map[string]float64) float64 {
	if len(factors) == 0 {
		return 1
	}
	if len(factors) == 1 {
		return getMapFirstValueSF(factors)
	}
	v, ok := factors["ALL"]
	if ok {
		return v
	}
	return 1
}

func singleToMapSF(value float64) map[string]float64 {
	return map[string]float64{
		"ALL": value,
	}
}

// 设置多屏的缩放比例的关键方法，factors 中必须含有主屏的数据。
func (m *XSManager) setScreenScaleFactors(factors map[string]float64, emitSignal bool) error {
	logger.Debug("setScreenScaleFactors", factors)
	for _, f := range factors {
		if f <= 0 {
			return errors.New("invalid value")
		}
	}
	if len(factors) == 0 {
		return errors.New("factors is empty")
	}

	err := m.dsfHelper.SetScaleFactors(factors)
	if err != nil {
		logger.Warning(err)
	}

	// 同时要设置单值的
	singleFactor := getSingleScaleFactor(factors)
	m.setScaleFactor(singleFactor, emitSignal)

	// 关键保存位置
	factorsJoined := joinScreenScaleFactors(factors)
	m.gs.SetString(gsKeyIndividualScaling, factorsJoined)

	err = m.setScreenScaleFactorsForQt(factors)
	if err != nil {
		return err
	}

	err = cleanUpDdeEnv()
	if err != nil {
		logger.Warning("failed to clean up dde env", err)
	}

	return err
}

func (m *XSManager) getScreenScaleFactors() map[string]float64 {
	factorsJoined := m.gs.GetString(gsKeyIndividualScaling)
	return parseScreenFactors(factorsJoined)
}

const plymouthConfigFile = "/etc/plymouth/plymouthd.conf"

func (m *XSManager) setScaleFactorForPlymouthReal(factor int, emitSignal bool) {
	logger.Debug("scalePlymouth", factor)
	currentFactor := 0
	theme, err := getPlymouthTheme(plymouthConfigFile)
	if err == nil {
		currentFactor = getPlymouthThemeScaleFactor(theme)
	} else {
		logger.Warning(err)
	}

	if currentFactor == factor {
		logger.Debug("quick end scalePlymouth", factor)
		m.emitSignalSetScaleFactor(true, emitSignal)
		return
	}

	m.emitSignalSetScaleFactor(false, emitSignal)
	err = m.sysDaemon.ScalePlymouth(0, uint32(factor))
	m.emitSignalSetScaleFactor(true, emitSignal)

	logger.Debug("end scalePlymouth", factor)
	if err != nil {
		logger.Warning(err)
	}
}

func (m *XSManager) emitSignalSetScaleFactor(done, emitSignal bool) {
	if !emitSignal {
		return
	}
	signalName := "SetScaleFactorStarted"
	if done {
		signalName = "SetScaleFactorDone"
	}
	err := m.service.Emit(m, signalName)
	if err != nil {
		logger.Warning(err)
	}
}

func (m *XSManager) startScaleFactorForPlymouth(factor int, emitSignal bool) {
	logger.Debug("startScaleFactorForPlymouth", factor)
	go func() {
		m.setScaleFactorForPlymouthReal(factor, emitSignal)
		m.endScaleFactorForPlymouth()
	}()
}

func (m *XSManager) endScaleFactorForPlymouth() {
	m.plymouthScalingMu.Lock()
	defer m.plymouthScalingMu.Unlock()

	if len(m.plymouthScalingTasks) == 0 {
		// stop
		m.plymouthScaling = false
	} else {
		factor := m.plymouthScalingTasks[len(m.plymouthScalingTasks)-1]
		logger.Debug("use last in tasks:", factor, m.plymouthScalingTasks)
		m.plymouthScalingTasks = nil
		m.startScaleFactorForPlymouth(factor, true)
	}
}

func (m *XSManager) setScaleFactorForPlymouth(factor int, emitSignal bool) {
	if factor > 2 {
		factor = 2
	}
	m.plymouthScalingMu.Lock()

	if m.plymouthScaling {
		m.plymouthScalingTasks = append(m.plymouthScalingTasks, factor)
		logger.Debug("add to tasks", factor)
	} else {
		m.plymouthScaling = true
		m.startScaleFactorForPlymouth(factor, emitSignal)
	}

	m.plymouthScalingMu.Unlock()
}

func getPlymouthTheme(file string) (string, error) {
	var kf = keyfile.NewKeyFile()
	err := kf.LoadFromFile(file)
	if err != nil {
		return "", err
	}

	return kf.GetString("Daemon", "Theme")
}

func getPlymouthThemeScaleFactor(theme string) int {
	switch theme {
	case "deepin-logo", "deepin-ssd-logo", "uos-ssd-logo":
		return 1
	case "deepin-hidpi-logo", "deepin-hidpi-ssd-logo", "uos-hidpi-ssd-logo":
		return 2
	default:
		return 0
	}
}

func (m *XSManager) updateGreeterQtTheme(kf *keyfile.KeyFile) error {
	tempFile, err := ioutil.TempFile("", "startdde-qt-theme-")
	if err != nil {
		return err
	}
	defer func() {
		err := tempFile.Close()
		if err != nil {
			logger.Warning(err)
		}
		err = os.Remove(tempFile.Name())
		if err != nil {
			logger.Warning(err)
		}
	}()

	kf.SetValue(qtThemeSection, qtThemeKeyScaleLogicalDpi, "96,96")
	err = kf.SaveToWriter(tempFile)
	if err != nil {
		return err
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	err = m.greeter.UpdateGreeterQtTheme(0, dbus.UnixFD(tempFile.Fd()))
	return err
}
