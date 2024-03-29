# 载荷发生器
由Go并发编程实战第二版学习而来。
核心功能是控制和协调载荷的生成和发送、响应的接收和验证，以及最终结果的提交等一系列动作。
拓展组件功能，自定义调用被测软件的请求生成和响应的检查操作。

## 构建思路

### 1. 基本结构

> 载荷考察参数

- 处理超时时间
- lps
- 负载持续时间

> 状态控制

- 载荷并发量
- goroutine票池
- 上下文控制

> 状态

- 状态
- 调用计数
- 调用结果通道

### 2. 功能

- 启动后，按照给定的参数向被测软件发送一定量的载荷
- 触达指定的负载持续时间后，自动停止载荷发送操作
- 启动到停止的时间还会将被测软件对各个载荷的响应的最终结果收集起来，发送到提供的调用通道

### 3. 启动

- 初始化参数、超时上下文、计时器
- 启动goroutine多路复用监听计时器和上下文通道
- 启动goroutine处理发送逻辑

  - 生成载荷

  - 发送载荷并接收响应

  - 检查载荷响应

  - 生成调用结果

  - 发送调用结果

### 4. 停止

- 超时自动停止
  - context.WithTimeout
- 手动停止
  - 执行ctx.cancelFunc
  - 查询发送器状态

##  调用器

### 服务

- 给某个端口起一个服务，功能：不断接收运算符请求，自动运算并返回响应

### 调用器

#### 1. 基本结构

- 服务地址

#### 2. 功能

1. 生成请求

   利用随机数随机生成

2. 调用服务

   写入和读取数据

3. 检查响应

   构造响应结果

   - ID
   - 原生请求
   - 原生响应
   - 响应代码
   - 响应结果原因描述
   - 耗时

## 程序测试结果

### mac电脑

配置：m1芯片，16g内存

### Linux 服务器

配置：
