# Type-C 拔出导致 I2C6 总线挂起问题分析

## 问题现象

在 RK3588 AIoT-3588IED 板卡上，当 Type-C 数据线被拔出后：

1. I2C6 总线挂起（所有 I2C6 设备通信失败，返回 `-110 ETIMEDOUT`）
2. PCA9555 GPIO 扩展器失联 → PCIe3x4 PERST# 无法控制 → NVMe SSD 消失
3. SMDT 看门狗喂狗失败 → 硬件看门狗超时 → 系统自动重启

使用 BSP 5.10 内核 + 出厂 boot.dts 时，同样的硬件拔 Type-C 不会出现任何问题。

## 硬件拓扑

```
                     I2C6 (fec80000)
                     ┌──────────────────────────────────────────┐
                     │                                          │
  ┌──────────┐ SCL/SDA │  ┌─────────┐  ┌─────────┐  ┌────────┐ │
  │ fusb302  │──────────│  │nca9555  │  │hym8563  │  │smdt_wdt│ │
  │ @0x22    │          │  │@0x20    │  │@0x51    │  │@0x62   │ │
  │Type-C PD │          │  │GPIO扩展 │  │RTC      │  │看门狗  │ │
  └──────────┘          │  └────┬────┘  └─────────┘  └────────┘ │
       │ INT_N (GPIO0_D3)│       │ pin15                     │
       │                 └───────┼───────────────────────────┘
       ▼                         ▼
  TCPM 状态机              PCIe3x4 PERST#
  (通过 I2C 与              (通过 nca9555
   fusb302 通信)             GPIO pin15 控制)
```

**关键依赖链**：NVMe SSD → PCIe3x4 链路训练 → PERST# GPIO → nca9555 → I2C6 总线

当 I2C6 总线挂起时，nca9555 无法通信，PERST# 无法控制，PCIe3x4 链路训练失败，NVMe SSD 消失。同时看门狗也无法喂狗，最终导致系统重启。

## 根因分析

经过与出厂 boot.dts 逐项对比，发现 3 个关键 DTS 差异导致了此问题：

### 差异 1：usbdp_phy0 缺少 `rockchip,dp-lane-mux` 属性（根因）

#### boot.dts 配置

```dts
phy@fed80000 {  /* usbdp_phy0 */
    rockchip,dp-lane-mux = <0 1 2 3>;  /* 4 lane 全部分配给 DP */
    orientation-switch;
    ...
};
```

#### 我们之前的配置（错误）

```dts
&usbdp_phy0 {
    /* 缺少 rockchip,dp-lane-mux */
    orientation-switch;
    ...
};
```

#### 代码级分析

usbdp PHY 驱动在 `rk_udphy_parse_lane_mux_data()` 中解析此属性
（`drivers/phy/rockchip/phy-rockchip-usbdp.c:879`）：

```c
num_lanes = device_property_count_u32(udphy->dev, "rockchip,dp-lane-mux");
if (num_lanes < 0) {
    // 没有 dp-lane-mux 属性时
    udphy->mode = UDPHY_MODE_USB;  // ← 仅 USB 模式
    return 0;
}
// 有 dp-lane-mux = <0 1 2 3> 时
udphy->mode = UDPHY_MODE_DP;  // ← DP 模式（4 lane 全给 DP）
```

这个初始 `mode` 值决定了 USB3 PHY 初始化行为
（`rk_udphy_usb3_phy_init()` line 1303）：

```c
static int rk_udphy_usb3_phy_init(struct phy *phy)
{
    // 如果 mode 不包含 USB，或者 hs（高速模式），禁用 U3 端口
    if (!(udphy->mode & UDPHY_MODE_USB) || udphy->hs) {
        rk_udphy_u3_port_disable(udphy, true);  // ← 禁用 USB3
        goto unlock;
    }
    // 否则尝试 power on USB3
    ret = rk_udphy_power_on(udphy, UDPHY_MODE_USB);  // ← 启用 USB3
}
```

**有 `dp-lane-mux = <0 1 2 3>`（boot.dts）**：
- 初始 `mode = UDPHY_MODE_DP`（0x02）
- `UDPHY_MODE_DP & UDPHY_MODE_USB = 0` → USB3 U3 端口被**禁用**
- USB3 PHY 从不 power on，不会触碰 lane mux 寄存器
- Type-C 拔出时，PHY 状态稳定，不影响 I2C 总线

**无 `dp-lane-mux`（我们之前的配置）**：
- 初始 `mode = UDPHY_MODE_USB`（0x01）
- `UDPHY_MODE_USB & UDPHY_MODE_USB = 1` → USB3 U3 端口被**启用**
- `rk_udphy_power_on(udphy, UDPHY_MODE_USB)` 被调用
- USB3 PHY 尝试 power on，操作 lane mux、reset、clock 等寄存器
- Type-C 拔出时，TCPM 状态机通过 I2C 与 fusb302 通信处理断开事件，
  同时 usbdp_phy0 通过 typec orientation switch 收到 `TYPEC_ORIENTATION_NONE`，
  触发 PHY lane 重新配置
- USB3 PHY 的 power off/on 操作与 fusb302 的 I2C 通信同时发生，
  可能导致 PHY 电气状态变化，影响 I2C6 总线信号完整性

#### 为什么 `dp-lane-mux = <0 1 2 3>` 是正确的

AIoT-3588IED 板卡的 Type-C 接口通过 usbdp_phy0 连接，但硬件设计上
**所有 4 条 lane 都用于 DisplayPort**，USB3 SuperSpeed 信号不走 Type-C 通道。
Type-C 端口仅支持 USB2.0（通过 u2phy0_otg → DWC3），不支持 USB3 SS。

因此 `dp-lane-mux = <0 1 2 3>` 准确反映了硬件设计，让 PHY 驱动知道
USB3 功能不需要启用，避免不必要的 PHY 操作。

### 差异 2：fusb302 connector 端口拓扑不匹配

#### boot.dts 拓扑

```
fusb302@22
├── ports              ← 顶层 ports，用于 USB role switch
│   └── port@0
│       └── endpoint@0 ──→ DWC3 usb@fc000000 port endpoint
│                            (USB2 HS 通道，角色切换)
└── connector
    └── ports          ← connector ports，用于 DP alt-mode
        ├── port@0
        │   └── endpoint ──→ usbdp_phy0 endpoint@0 (USB3 SS)
        └── port@1
            └── endpoint ──→ usbdp_phy0 endpoint@1 (DP)
```

#### 我们之前的拓扑（错误）

```
fusb302@22
└── connector
    └── ports
        ├── port@0 ──→ DWC3 usb@fc000000 (HS)  ← 错误：HS 应该走顶层 ports
        └── port@1 ──→ usbdp_phy0 endpoint@0 (SS) ← 错误：缺少 DP endpoint
```

#### 影响

1. **USB role switch 连接路径错误**：
   - boot.dts：DWC3 通过 fusb302 顶层 `ports` 节点找到 role switch 对端
   - 我们的：DWC3 通过 connector `ports` 节点连接，路径不同
   - 主线内核 DWC3 驱动使用 fwnode 匹配注册 role switch，
     但 fusb302 驱动在 `fusb302_fwnode_get()` 中查找 `connector` 子节点
     作为 TCPM fwnode，connector 内的 ports 用于 DP alt-mode bridge

2. **缺少 DP alt-mode 连接**：
   - boot.dts：usbdp_phy0 有 endpoint@0 (SS) 和 endpoint@1 (DP)
   - 我们的：只有 endpoint@0 (SS)，没有 endpoint@1 (DP)
   - 导致 typec_mux_set 无法正确路由 DP alt-mode 状态到 PHY

3. **TCPM 状态机行为异常**：
   - 拔出 Type-C 时，TCPM 调用 `tcpm_mux_set()` 传入
     `TYPEC_STATE_SAFE, USB_ROLE_NONE, TYPEC_ORIENTATION_NONE`
   - `typec_set_orientation()` → `rk_udphy_orien_sw_set()` 收到
     `TYPEC_ORIENTATION_NONE`
   - 在正确的拓扑下，此调用通过 OF graph 正确传播到 usbdp_phy0
   - 在错误拓扑下，可能触发异常的 PHY 状态转换

### 差异 3：PDO（Power Data Object）值不匹配

#### boot.dts

```dts
sink-pdos = <PDO_FIXED(5000, 1000, PDO_FIXED_USB_COMM)>;   /* 5V/1A 消费 */
source-pdos = <PDO_FIXED(5000, 3000, PDO_FIXED_USB_COMM)>; /* 5V/3A 供电 */
```

#### 我们之前的配置（错误）

```dts
sink-pdos = <PDO_FIXED(5000, 3000, PDO_FIXED_USB_COMM)>;   /* 5V/3A 消费 */
source-pdos = <PDO_FIXED(5000, 2000, PDO_FIXED_USB_COMM)>; /* 5V/2A 供电 */
```

#### 影响

- sink/source 电流值与出厂配置相反
- 板卡作为 sink 时声明需要 3A（实际硬件只支持 1A），
  作为 source 时声明提供 2A（实际硬件支持 3A）
- 错误的 PDO 可能导致 PD 协商失败，TCPM 触发 hard reset
- Hard reset 过程中，fusb302 需要通过 I2C 执行
  `fusb302_pd_send_hardreset()` → `fusb302_pd_reset()` 等操作，
  增加了 I2C 通信量和时序压力

### 其他差异（影响较小但已修正）

| 属性 | boot.dts | 我们之前 | 修正 |
|------|----------|----------|------|
| `pd-revision` | 无 | 有 (PD Rev 2.0) | 已删除 |
| `typec-power-opmode` | 无 | "1.5A" | 已删除 |
| hym8563 `interrupts` | 无 | 有 | 已删除 |
| hym8563 `clock-frequency` | 32768 | 无 | 已添加 |
| nca9555 `compatible` | `novosense,nca9555` | `nxp,pca9555` | 保持不变* |

\* 主线内核不支持 `novosense,nca9555`，使用 `nxp,pca9555` 兼容。
NCA9555 是 PCA9555 的国产替代，寄存器兼容。

## 问题触发的完整调用链

### 拔出 Type-C 时的内核调用链

```
用户拔出 Type-C 线
       │
       ▼
fusb302 芯片 INT_N 引脚拉低（中断触发）
       │
       ▼
fusb302_irq_intn() [hard IRQ]
       │ disable_irq_nosync()
       │ schedule_work(&chip->irq_work)
       ▼
fusb302_irq_work() [workqueue]
       │ fusb302_i2c_read(FUSB_REG_INTERRUPT)     ← I2C 读取
       │ fusb302_i2c_read(FUSB_REG_INTERRUPTA)    ← I2C 读取
       │ fusb302_i2c_read(FUSB_REG_INTERRUPTB)    ← I2C 读取
       │ fusb302_i2c_read(FUSB_REG_STATUS0)       ← I2C 读取
       │
       │ 检测到 COMP_CHNG 中断（CC 状态变化）
       │ tcpm_cc_change(chip->tcpm_port)
       ▼
TCPM 状态机处理 [tcpm.c]
       │ tcpm_detach(port)
       │   ├── tcpm_unregister_altmodes()
       │   ├── tcpm_typec_disconnect()
       │   ├── tcpm_mux_set(port, TYPEC_STATE_SAFE,
       │   │                 USB_ROLE_NONE,
       │   │                 TYPEC_ORIENTATION_NONE)
       │   │     ├── typec_set_orientation(NONE)
       │   │     │     └── typec_switch_set(sw, NONE)
       │   │     │           └── rk_udphy_orien_sw_set(NONE)
       │   │     │                 ├── gpiod_set_value(sbu1, 0)
       │   │     │                 ├── gpiod_set_value(sbu2, 0)
       │   │     │                 └── rk_udphy_usb_bvalid_enable(false)
       │   │     │                       └── grf 写 bvalid = 0
       │   │     ├── usb_role_switch_set_role(role_sw, NONE)
       │   │     │     └── dwc3_usb_role_switch_set(NONE)
       │   │     │           └── DWC3 切换到 device/host 模式
       │   │     └── typec_set_mode(port, TYPEC_STATE_SAFE)
       │   │
       │   └── tcpm_set_attached_state(false)
       │
       │ TCPM 继续状态转换...
       │ 可能触发 HARD_RESET_SEND
       │   └── tcpm_pd_transmit(TCPC_TX_HARD_RESET)
       │         └── fusb302_pd_send_hardreset()
       │               └── fusb302_i2c_set_bits(FUSB_REG_CONTROL3,
       │                                        SEND_HARDRESET)  ← I2C 写入
       │
       ▼
```

### I2C 总线挂起的机制

在没有 `rockchip,dp-lane-mux` 的情况下：

1. **USB3 PHY 处于活跃状态**：`UDPHY_MODE_USB` 导致 `rk_udphy_usb3_phy_init()`
   调用 `rk_udphy_power_on(udphy, UDPHY_MODE_USB)`，PHY 的 USB3 通道已上电

2. **Type-C 拔出触发多路并行操作**：
   - fusb302 通过 I2C 读取中断寄存器（4 次 I2C 读）
   - TCPM 状态机通过 I2C 发送 PD 消息（hard reset 等）
   - usbdp_phy0 通过 typec switch 收到 `ORIENTATION_NONE`，关闭 bvalid
   - DWC3 通过 role switch 收到 `USB_ROLE_NONE`，切换控制器模式
   - USB3 PHY 可能在此时尝试 power off

3. **信号完整性问题**：
   - usbdp_phy0 与 u2phy0 共享同一物理 Type-C 连接器
   - USB3 PHY power off/on 涉及高速差分信号线的电气状态变化
   - 这些高速信号线与 I2C6 的 SCL/SDA 在 PCB 布线上可能有耦合
   - 当 USB3 PHY 在没有正确 lane mux 配置的情况下尝试操作 lane 时，
     可能产生电气噪声，干扰 I2C6 总线信号

4. **fusb302 I2C 传输超时**：
   - I2C 传输在 SCL 被干扰时无法完成
   - rk3x-i2c 驱动等待 1 秒后超时，返回 `-ETIMEDOUT`
   - fusb302 驱动无法完成中断处理
   - `enable_irq()` 在 `done:` 标签处被调用，IRQ 重新启用
   - fusb302 INT_N 仍然为低（中断未清除），IRQ 立即重新触发
   - 形成 1 秒周期的 I2C 失败循环，总线持续被干扰

5. **级联失败**：
   - nca9555 通信失败 → PERST# 无法控制 → PCIe3x4 链路掉
   - smdt_wdt 喂狗失败 → 硬件看门狗超时 → 系统重启

### 有 `rockchip,dp-lane-mux = <0 1 2 3>` 时为什么不会挂起

1. **USB3 PHY 从不上电**：`UDPHY_MODE_DP` 使得 `rk_udphy_usb3_phy_init()`
   直接调用 `rk_udphy_u3_port_disable(udphy, true)`，U3 端口被禁用

2. **没有 USB3 lane 操作**：PHY 从不调用 `rk_udphy_power_on(USB)`，
   不操作 lane mux 寄存器，不涉及高速差分信号线的状态切换

3. **Type-C 拔出时操作最小化**：
   - `rk_udphy_orien_sw_set(NONE)` 只做 SBU GPIO 清零和 bvalid 禁用
   - 不涉及 PHY power off/on，不涉及 lane 重新配置
   - 电气状态稳定，不影响 I2C6 总线

4. **fusb302 I2C 通信正常**：
   - I2C 总线信号不受干扰
   - fusb302 中断处理正常完成
   - 读取中断寄存器、TCPM 状态转换、PD 通信都正常
   - 所有 I2C6 设备（nca9555、hym8563、smdt_wdt）正常工作

## 修复方案

### DTS 修改（已实施）

#### 1. usbdp_phy0 添加 dp-lane-mux 和双端点

```dts
&usbdp_phy0 {
    rockchip,dp-lane-mux = <0 1 2 3>;  /* 4 lane 全分配给 DP */
    orientation-switch;
    sbu1-dc-gpios = <&gpio4 RK_PB1 GPIO_ACTIVE_HIGH>;
    sbu2-dc-gpios = <&gpio0 RK_PD1 GPIO_ACTIVE_HIGH>;
    status = "okay";

    port {
        #address-cells = <1>;
        #size-cells = <0>;

        usbdp_phy0_typec_ss: endpoint@0 {
            reg = <0>;
            remote-endpoint = <&usbc0_ss>;
        };

        usbdp_phy0_typec_dp: endpoint@1 {
            reg = <1>;
            remote-endpoint = <&usbc0_dp>;
        };
    };
};
```

#### 2. fusb302 添加顶层 ports，修正 connector 拓扑

```dts
fusb302@22 {
    compatible = "fcs,fusb302";
    reg = <0x22>;
    ...
    status = "okay";

    /* 顶层 ports：USB role switch 连接（HS → DWC3） */
    ports {
        #address-cells = <1>;
        #size-cells = <0>;

        port@0 {
            reg = <0>;
            usbc0_hs: endpoint@0 {
                remote-endpoint = <&usb_host0_xhci_drd_sw>;
            };
        };
    };

    connector {
        compatible = "usb-c-connector";
        label = "USB-C";
        data-role = "dual";
        power-role = "dual";
        try-power-role = "sink";
        op-sink-microwatt = <1000000>;
        /* PDO 值与 boot.dts 一致 */
        sink-pdos = <PDO_FIXED(5000, 1000, PDO_FIXED_USB_COMM)>;
        source-pdos = <PDO_FIXED(5000, 3000, PDO_FIXED_USB_COMM)>;

        altmodes { ... };

        /* connector ports：DP alt-mode 连接（SS/DP → usbdp_phy0） */
        ports {
            #address-cells = <1>;
            #size-cells = <0>;

            port@0 {
                reg = <0>;
                usbc0_ss: endpoint {
                    remote-endpoint = <&usbdp_phy0_typec_ss>;
                };
            };
            port@1 {
                reg = <1>;
                usbc0_dp: endpoint {
                    remote-endpoint = <&usbdp_phy0_typec_dp>;
                };
            };
        };
    };
};
```

#### 3. hym8563 与 boot.dts 对齐

```dts
hym8563: rtc@51 {
    compatible = "haoyu,hym8563";
    reg = <0x51>;
    #clock-cells = <0>;
    clock-frequency = <32768>;        /* 新增 */
    clock-output-names = "hym8563";
    wakeup-source;
    /* 删除 interrupts / pinctrl-0 / interrupt-parent */
};
```

#### 4. DWC3 xHCI 保持 usb-role-switch + port 连接到 fusb302 顶层 ports

```dts
&usb_host0_xhci {
    usb-role-switch;
    status = "okay";

    port {
        usb_host0_xhci_drd_sw: endpoint {
            remote-endpoint = <&usbc0_hs>;  /* → fusb302 顶层 ports port@0 */
        };
    };
};
```

### 修改总结

| 修改项 | 修改前 | 修改后 | 影响 |
|--------|--------|--------|------|
| `usbdp_phy0` `rockchip,dp-lane-mux` | 无 | `<0 1 2 3>` | PHY 初始模式 = DP_ONLY，禁用 USB3，避免拔 Type-C 时 PHY lane 操作干扰 I2C |
| `usbdp_phy0` port | 仅 endpoint@0 | endpoint@0 + endpoint@1 | 完整的 SS + DP alt-mode 连接 |
| `fusb302` 顶层 `ports` | 无 | port@0 → DWC3 | 正确的 USB role switch OF graph 路径 |
| `fusb302` connector ports | port@0→DWC3, port@1→SS | port@0→SS, port@1→DP | 正确的 DP alt-mode 连接拓扑 |
| `fusb302` sink-pdos | 5V/3A | 5V/1A | 与硬件一致 |
| `fusb302` source-pdos | 5V/2A | 5V/3A | 与硬件一致 |
| `fusb302` pd-revision | 有 | 删除 | boot.dts 无此属性 |
| `fusb302` typec-power-opmode | "1.5A" | 删除 | boot.dts 无此属性 |
| `hym8563` interrupts | 有 | 删除 | boot.dts 无此属性 |
| `hym8563` clock-frequency | 无 | 32768 | 与 boot.dts 一致 |

## 验证方法

1. **启动时（Type-C 连接）**：
   - `dmesg | grep i2c` — 不应有 `ipd = 0x30` 警告
   - `dmesg | grep fusb302` — fusb302 驱动正常 probe
   - `dmesg | grep pca953x` — nca9555 正常 probe
   - `lsblk` — NVMe SSD 可见
   - `dmesg | grep smdt` — 看门狗正常启动

2. **运行时（Type-C 拔出）**：
   - `dmesg` — USB disconnect 正常，无 I2C timeout
   - `lsblk` — NVMe SSD 仍然可见
   - `dmesg | grep smdt` — 看门狗喂狗正常
   - 系统不重启

3. **运行时（Type-C 重新插入）**：
   - USB 设备重新枚举
   - 所有功能恢复正常

## 参考文件

- `factory-image/boot.dts` — 出厂 BSP 5.10 内核 DTS（权威硬件参考）
- `drivers/phy/rockchip/phy-rockchip-usbdp.c` — usbdp PHY 驱动
  - `rk_udphy_parse_lane_mux_data()` (line 879) — dp-lane-mux 解析
  - `rk_udphy_usb3_phy_init()` (line 1303) — USB3 PHY 初始化
  - `rk_udphy_orien_sw_set()` (line 654) — Type-C 方向切换处理
  - `rk_udphy_set_typec_default_mapping()` (line 621) — lane 默认映射
  - `rk_udphy_init()` (line 782) — PHY 初始化序列
- `drivers/usb/typec/tcpm/fusb302.c` — fusb302 Type-C PD 控制器驱动
  - `fusb302_irq_work()` (line 1502) — 中断处理工作队列
  - `fusb302_fwnode_get()` (line 1678) — connector fwnode 获取
- `drivers/usb/typec/tcpm/tcpm.c` — TCPM 状态机
  - `tcpm_detach()` (line 4603) — Type-C 断开处理
  - `tcpm_mux_set()` (line 1090) — mux/switch 设置
- `drivers/i2c/busses/i2c-rk3x.c` — Rockchip I2C 总线驱动
  - `rk3x_i2c_xfer_common()` (line 1062) — I2C 传输主函数
  - 超时处理 (line 1112) — 强制 STOP + 返回 ETIMEDOUT
- `drivers/usb/dwc3/drd.c` — DWC3 双角色驱动
  - `dwc3_setup_role_switch()` (line 500) — role switch 注册
