package orsql

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"

	openrasp "github.com/baidu-security/openrasp-golang"
	"github.com/baidu-security/openrasp-golang/gls"
	"github.com/baidu-security/openrasp-golang/model"
	"github.com/baidu-security/openrasp-golang/stacktrace"
	"github.com/baidu-security/openrasp-golang/support/orhttp"
	"github.com/baidu-security/openrasp-golang/utils"
)

var (
	driversMu sync.RWMutex
	drivers   = make(map[string]*wrapDriver)
)

type DSNParserFunc func(dsn string) DSNInfo
type ErrorInterceptorFunc func(err *error) (bool, string, string)

func genericDSNParser(string) DSNInfo {
	return DSNInfo{}
}

func genericErrorInterceptor(err *error) (bool, string, string) {
	return false, "", ""
}

func Register(name string, driver driver.Driver, opts ...WrapOption) {
	driversMu.Lock()
	defer driversMu.Unlock()

	wrapped := newWrapDriver(driver, opts...)
	sql.Register(wrapDriverName(name), wrapped)
	drivers[name] = wrapped
}

func wrapDriverName(origin string) string {
	return "openrasp/" + origin
}

func sqlConnectionPolicyCheck(d *wrapDriver, name string) (model.InterceptCode, string) {
	dsnInfo := d.dsnParser(name)
	dbConnParam := NewDbConnectionParam(&dsnInfo, d.driverName)
	interceptCode, policyResult := dbConnParam.PolicyCheck()
	var policyLogString string
	if interceptCode != model.Ignore {
		policyLog := model.PolicyLog{
			PolicyResult: policyResult,
			Server:       openrasp.GetGlobals().Server,
			System:       openrasp.GetGlobals().System,
			PolicyParams: dbConnParam,
			SourceCode:   []string{},
			StackTrace:   strings.Join(stacktrace.LogFormat(stacktrace.AppendStacktrace(nil, 1, openrasp.GetGeneral().GetInt("log.maxstack"))), "\n"),
			RaspId:       openrasp.GetGlobals().RaspId,
			AppId:        openrasp.GetBasic().GetString("cloud.app_id"),
			EventTime:    utils.CurrentISO8601Time(),
		}
		policyLogString = policyLog.String()
	}
	return interceptCode, policyLogString
}

func Open(driverName, dataSourceName string) (*sql.DB, error) {
	if openrasp.IsComplete() && gls.Activated() {
		d, ok := drivers[driverName]
		var interceptCode model.InterceptCode = model.Ignore
		var policyLogString string
		if ok {
			interceptCode, policyLogString = sqlConnectionPolicyCheck(d, dataSourceName)
			if interceptCode == model.Block {
				if len(policyLogString) > 0 {
					openrasp.GetLog().PolicyInfo(policyLogString)
				}
				blocker, ok := gls.Get("responseWriter").(orhttp.OpenRASPBlocker)
				if ok {
					blocker.BlockByOpenRASP()
				}
			}
		}
		db, err := sql.Open(wrapDriverName(driverName), dataSourceName)
		if err != nil {
			d.interceptError(dataSourceName, &err)
			return nil, err
		} else {
			if interceptCode == model.Log {
				openrasp.GetLog().PolicyInfo(policyLogString)
			}
		}
		return db, err
	}
	return sql.Open(driverName, dataSourceName)
}

func Wrap(driver driver.Driver, opts ...WrapOption) driver.Driver {
	return newWrapDriver(driver, opts...)
}

func newWrapDriver(driver driver.Driver, opts ...WrapOption) *wrapDriver {
	d := &wrapDriver{
		Driver: driver,
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.driverName == "" {
		d.driverName = ExtractName(driver)
	}
	if d.dsnParser == nil {
		d.dsnParser = genericDSNParser
	}
	if d.errorInterceptor == nil {
		d.errorInterceptor = genericErrorInterceptor
	}
	return d
}

func DriverDSNParser(driverName string) DSNParserFunc {
	driversMu.RLock()
	driver := drivers[driverName]
	defer driversMu.RUnlock()
	return driver.dsnParser
}

type WrapOption func(*wrapDriver)

func DriverNameWrap(name string) WrapOption {
	return func(d *wrapDriver) {
		d.driverName = name
	}
}

func DSNParserWrap(f DSNParserFunc) WrapOption {
	return func(d *wrapDriver) {
		d.dsnParser = f
	}
}

func ErrorInterceptorWrap(f ErrorInterceptorFunc) WrapOption {
	return func(d *wrapDriver) {
		d.errorInterceptor = f
	}
}

type wrapDriver struct {
	driver.Driver
	driverName       string
	dsnParser        DSNParserFunc
	errorInterceptor ErrorInterceptorFunc
}

func (d *wrapDriver) interceptError(param string, err *error) {
	hit, errCode, errMsg := d.errorInterceptor(err)
	if hit {
		sqlErrorParam := NewSqlErrorParam(d.driverName, param, errCode, errMsg)
		shouldBlock := false
		requestInfo, ok := gls.Get("requestInfo").(*model.RequestInfo)
		if ok {
			attackResults := sqlErrorParam.AttackCheck(openrasp.IgnoreActionOption, openrasp.WhitelistOption)
			for _, attackResult := range attackResults {
				if interceptCode := attackResult.GetInterceptState(); interceptCode != model.Ignore {
					attackLog := model.AttackLog{
						AttackResult: attackResult,
						Server:       openrasp.GetGlobals().Server,
						System:       openrasp.GetGlobals().System,
						RequestInfo:  requestInfo,
						AttackParams: sqlErrorParam,
						SourceCode:   []string{},
						StackTrace:   strings.Join(stacktrace.LogFormat(stacktrace.AppendStacktrace(nil, 1, openrasp.GetGeneral().GetInt("log.maxstack"))), "\n"),
						RaspId:       openrasp.GetGlobals().RaspId,
						AppId:        openrasp.GetBasic().GetString("cloud.app_id"),
						ServerIp:     openrasp.GetGlobals().HttpAddr,
						EventTime:    utils.CurrentISO8601Time(),
						EventType:    "attack",
						AttackType:   sqlErrorParam.GetTypeString(),
					}
					attackLogString := attackLog.String()
					if len(attackLogString) > 0 {
						openrasp.GetLog().AlarmInfo(attackLogString)
					}
					if interceptCode == model.Block {
						shouldBlock = true
					}
				}
			}
		}
		if shouldBlock {
			blocker, ok := gls.Get("responseWriter").(orhttp.OpenRASPBlocker)
			if ok {
				blocker.BlockByOpenRASP()
			}
		}
	}
}

func (d *wrapDriver) Open(name string) (driver.Conn, error) {
	dsnInfo := d.dsnParser(name)
	interceptCode, policyLogString := sqlConnectionPolicyCheck(d, name)
	if interceptCode == model.Block {
		if len(policyLogString) > 0 {
			openrasp.GetLog().PolicyInfo(policyLogString)
		}
		panic(openrasp.ErrBlock)
	}
	conn, err := d.Driver.Open(name)
	if err != nil {
		d.interceptError(name, &err)
		return nil, err
	} else {
		if interceptCode == model.Log {
			openrasp.GetLog().PolicyInfo(policyLogString)
		}
	}
	return newConn(conn, d, dsnInfo), nil
}

func namedValueToValue(named []driver.NamedValue) ([]driver.Value, error) {
	dargs := make([]driver.Value, len(named))
	for n, param := range named {
		if len(param.Name) > 0 {
			return nil, errors.New("sql: driver does not support the use of Named Parameters")
		}
		dargs[n] = param.Value
	}
	return dargs, nil
}

type namedValueChecker interface {
	CheckNamedValue(*driver.NamedValue) error
}

func checkNamedValue(nv *driver.NamedValue, next namedValueChecker) error {
	if next != nil {
		return next.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}
