package main

import (
    "fmt"
    "myuia/libs"
)

func main() {
    d, err := libs.NewDevice("192.168.31.182:5555")
    if err != nil {
        panic(err)
    }
    defer d.Close()

    el := d.By(libs.P{
        "resourceId": "com.miui.home:id/icon_title",
        "text":       "支付宝",
    })

    // 等待元素出现（最多10秒）
    if err := el.MustWait(10.0); err != nil {
        fmt.Println("元素未找到:", err)
        return
    }

    if err := el.Click(); err != nil {
        fmt.Println("点击失败:", err)
        return
    }

    fmt.Println("点击支付宝成功")
}
