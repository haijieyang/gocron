package service

import (
    "github.com/ouqiang/gocron/models"
    "strconv"
    "time"
    "github.com/ouqiang/gocron/modules/logger"
    "github.com/ouqiang/gocron/modules/ssh"
    "github.com/jakecoffman/cron"
    "github.com/ouqiang/gocron/modules/utils"
    "errors"
    "fmt"
    "github.com/ouqiang/gocron/modules/httpclient"
    "github.com/ouqiang/gocron/modules/notify"
    "sync"
)

// 定时任务调度管理器
var Cron *cron.Cron
// 同一任务是否有实例处于运行中
var runInstance Instance
// 任务计数-正在运行中的任务
var TaskNum TaskCount

// 任务计数
type TaskCount struct {
    num int
    sync.RWMutex
}

func (c *TaskCount) Add()  {
    c.Lock()
    defer c.Unlock()
    c.num += 1
}

func (c *TaskCount) Done()  {
    c.Lock()
    defer c.Unlock()
    c.num -= 1
}

func (c *TaskCount) Num() int  {
    c.RLock()
    defer c.RUnlock()

    return c.num
}

// 任务ID作为Key, 不会出现并发写, 不加锁
type Instance struct {
    Status map[int]bool
}

// 是否有任务处于运行中
func (i *Instance) has(key int) bool {
    running, ok := i.Status[key]
    if ok && running {
        return true
    }

    return false
}

func (i *Instance) add(key int)  {
    i.Status[key] = true
}

func (i *Instance) done(key int)  {
    i.Status[key] = false
}

type Task struct{}

type TaskResult struct {
    Result string
    Err error
    RetryTimes int8
    IsAsync bool
}

// 初始化任务, 从数据库取出所有任务, 添加到定时任务并运行
func (task *Task) Initialize() {
    Cron = cron.New()
    Cron.Start()
    runInstance = Instance{make(map[int]bool)}
    TaskNum = TaskCount{0, sync.RWMutex{}}

    taskModel := new(models.Task)
    taskList, err := taskModel.ActiveList()
    if err != nil {
        logger.Error("定时任务初始化#获取任务列表错误-", err.Error())
        return
    }
    if len(taskList) == 0 {
        logger.Debug("任务列表为空")
        return
    }
    task.BatchAdd(taskList)
}

// 批量添加任务
func (task *Task) BatchAdd(tasks []models.TaskHost)  {
    for _, item := range tasks {
        task.Add(item)
    }
}

// 添加任务
func (task *Task) Add(taskModel models.TaskHost) {
    taskFunc := createJob(taskModel)
    if taskFunc == nil {
        logger.Error("创建任务处理Job失败,不支持的任务协议#", taskModel.Protocol)
        return
    }

    cronName := strconv.Itoa(taskModel.Id)
    // Cron任务采用数组存储, 删除任务需遍历数组, 并对数组重新赋值, 任务较多时，有性能问题
    Cron.RemoveJob(cronName)
    err := Cron.AddFunc(taskModel.Spec, taskFunc, cronName)
    if err != nil {
        logger.Error("添加任务到调度器失败#", err)
    }
}

// 停止所有任务
func (task *Task) StopAll()  {
    Cron.Stop()
}

// 直接运行任务
func (task *Task) Run(taskModel models.TaskHost)  {
    go createJob(taskModel)()
}

type Handler interface {
    Run(taskModel models.TaskHost) (string, error)
}

// 本地命令
type LocalCommandHandler struct {}

// 运行本地命令
func (h *LocalCommandHandler) Run(taskModel models.TaskHost) (string, error)  {
    if taskModel.Command == "" {
        return "", errors.New("invalid command")
    }

    if utils.IsWindows() {
        return h.runOnWindows(taskModel)
    }

    return h.runOnUnix(taskModel)
}

// 执行Windows命令
func (h *LocalCommandHandler) runOnWindows(taskModel models.TaskHost) (string, error) {
    outputGBK, err := utils.ExecShellWithTimeout(taskModel.Timeout, "cmd", "/C", taskModel.Command)
    // windows平台编码为gbk，需转换为utf8才能入库
    outputUTF8, ok := utils.GBK2UTF8(outputGBK)
    if ok {
        return outputUTF8, err
    }

    return "命令输出转换编码失败(gbk to utf8)", err
}

// 执行Unix命令
func (h *LocalCommandHandler) runOnUnix(taskModel models.TaskHost) (string, error)  {
    return utils.ExecShellWithTimeout(taskModel.Timeout, "/bin/bash", "-c", taskModel.Command)
}

// HTTP任务
type HTTPHandler struct{}

func (h *HTTPHandler) Run(taskModel models.TaskHost) (result string, err error) {
    resp := httpclient.Get(taskModel.Command, taskModel.Timeout)
    // 返回状态码非200，均为失败
    if resp.StatusCode != 200 {
        return resp.Body, errors.New(fmt.Sprintf("HTTP状态码非200-->%d", resp.StatusCode))
    }

    return resp.Body, err
}

// SSH-command任务
type SSHCommandHandler struct{}

func (h *SSHCommandHandler) Run(taskModel models.TaskHost) (string, error) {
    hostModel := new(models.Host)
    err := hostModel.Find(int(taskModel.HostId))
    if err != nil {
        return "", err
    }
    sshConfig := ssh.SSHConfig{}
    sshConfig.User = hostModel.Username
    sshConfig.Host = hostModel.Name
    sshConfig.Port = hostModel.Port
    sshConfig.ExecTimeout = taskModel.Timeout
    sshConfig.AuthType = hostModel.AuthType
    var password string
    var privateKey string
    if hostModel.AuthType == ssh.HostPassword {
        password, err = hostModel.GetPasswordByHost(hostModel.Name)
        if err != nil {
            return "", err
        }
        sshConfig.Password = password
    } else {
        privateKey, err = hostModel.GetPrivateKeyByHost(hostModel.Name)
        if err != nil {
            return "", err
        }
        sshConfig.PrivateKey = privateKey
    }

    return ssh.Exec(sshConfig, taskModel.Command)
}

// 创建任务日志
func createTaskLog(taskModel models.TaskHost, status models.Status) (int64, string, error) {
    taskLogModel := new(models.TaskLog)
    taskLogModel.TaskId = taskModel.Id
    taskLogModel.Name = taskModel.Task.Name
    taskLogModel.Spec = taskModel.Spec
    taskLogModel.Protocol = taskModel.Protocol
    taskLogModel.Command = taskModel.Command
    taskLogModel.Timeout = taskModel.Timeout
    if taskModel.Protocol == models.TaskSSH {
        taskLogModel.Hostname = taskModel.Alias + "-" + taskModel.Name
    }
    taskLogModel.StartTime = time.Now()
    taskLogModel.Status = status
    // SSH执行远程命令，后台运行
    var notifyId string = ""
    if taskModel.Timeout == -1 {
        notifyId = utils.RandString(32);
        taskLogModel.NotifyId = notifyId;
    }
    insertId, err := taskLogModel.Create()

    return insertId, notifyId, err
}

// 更新任务日志
func updateTaskLog(taskLogId int64, taskResult TaskResult) (int64, error) {
    taskLogModel := new(models.TaskLog)
    var status models.Status
    var result string = taskResult.Result
    if taskResult.Err != nil {
        status = models.Failure
    } else if taskResult.IsAsync {
        status = models.Background
    } else {
        status = models.Finish
    }
    return taskLogModel.Update(taskLogId, models.CommonMap{
        "retry_times": taskResult.RetryTimes,
        "status": status,
        "result": result,
    })

}

func createJob(taskModel models.TaskHost) cron.FuncJob {
    var handler Handler = createHandler(taskModel)
    if handler == nil {
        return nil
    }
    taskFunc := func() {
        TaskNum.Add()
        defer TaskNum.Done()
        taskLogId := beforeExecJob(&taskModel)
        if taskLogId <= 0 {
            return
        }
        logger.Infof("开始执行任务#%s#命令-%s", taskModel.Task.Name, taskModel.Command)
        taskResult := execJob(handler, taskModel)
        logger.Infof("任务完成#%s#命令-%s", taskModel.Task.Name, taskModel.Command)
        afterExecJob(taskModel, taskResult, taskLogId)
    }

    return taskFunc
}

func createHandler(taskModel models.TaskHost) Handler  {
    var handler Handler = nil
    switch taskModel.Protocol {
        case models.TaskHTTP:
            handler = new(HTTPHandler)
        case models.TaskSSH:
            handler = new(SSHCommandHandler)
        case models.TaskLocalCommand:
            handler = new(LocalCommandHandler)
    }

    return handler;
}

func beforeExecJob(taskModel *models.TaskHost) (taskLogId int64)  {
    if taskModel.Multi == 0 && runInstance.has(taskModel.Id) {
        createTaskLog(*taskModel, models.Cancel)
        return
    }
    if taskModel.Multi == 0 {
        runInstance.add(taskModel.Id)
    }
    taskLogId, notifyId, err := createTaskLog(*taskModel, models.Running)
    if err != nil {
        logger.Error("任务开始执行#写入任务日志失败-", err)
        return
    }
    // 设置notifyId到环境变量中
    if notifyId != "" {
        envName := "GOCRON_TASK_ID"
        if taskModel.Protocol == models.TaskSSH {
            taskModel.Command = fmt.Sprintf("%s%s", utils.FormatUnixEnv(envName, notifyId), taskModel.Command)
        } else {
            taskModel.Command = fmt.Sprintf("%s%s", utils.FormatEnv(envName, notifyId), taskModel.Command)
        }
    }

    logger.Debugf("任务命令-%s", taskModel.Command)

    return taskLogId
}

func afterExecJob(taskModel models.TaskHost, taskResult TaskResult, taskLogId int64)  {
    if taskResult.Err != nil {
        taskResult.Result = taskResult.Err.Error() + "\n" + taskResult.Result
    }
    if taskModel.Timeout == -1 {
        taskResult.IsAsync = true
    }
    _, err := updateTaskLog(taskLogId, taskResult)
    if err != nil {
        logger.Error("任务结束#更新任务日志失败-", err)
    }
    if taskResult.IsAsync {
        return
    }

    sendNotification(taskModel, taskResult)
}

// 发送任务结果通知
func sendNotification(taskModel models.TaskHost, taskResult TaskResult)  {
    var statusName string
    // 未开启通知
    if taskModel.NotifyStatus == 0 {
        return
    }
    if taskModel.NotifyStatus == 1 && taskResult.Err == nil {
        // 执行失败才发送通知
        return
    }
    if taskResult.Err != nil {
        statusName = "失败"
    } else {
        statusName = "成功"
    }
    // 发送通知
    msg := notify.Message{
        "task_type": taskModel.NotifyType,
        "task_receiver_id": taskModel.NotifyReceiverId,
        "name": taskModel.Task.Name,
        "output": taskResult.Result,
        "status": statusName,
        "taskId": taskModel.Id,
    };
    notify.Push(msg)
}

// 执行具体任务
func execJob(handler Handler, taskModel models.TaskHost) TaskResult  {
    if taskModel.Multi == 0 {
        defer runInstance.done(taskModel.Id)
    }
    // 默认只运行任务一次
    var execTimes int8 = 1
    if (taskModel.RetryTimes > 0) {
        execTimes += taskModel.RetryTimes
    }
    var i int8 = 0
    var output string
    var err error
    for i < execTimes {
        output, err = handler.Run(taskModel)
        if err == nil {
            return TaskResult{Result: output, Err: err, RetryTimes: i}
        }
        i++
        if i < execTimes {
            logger.Warnf("任务执行失败#任务id-%d#重试第%d次#输出-%s#错误-%s", taskModel.Id, i, output, err.Error())
            // 重试间隔时间，每次递增1分钟
            time.Sleep( time.Duration(i) * time.Minute)
        }
    }

    return TaskResult{Result: output, Err: err, RetryTimes: taskModel.RetryTimes}
}