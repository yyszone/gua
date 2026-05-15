package main

import (
    "fmt"
    "time"
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

    t := time.Now()
    if err := el.MustWait(10.0); err != nil {
        fmt.Println("未找到:", err)
        return
    }
    fmt.Println("MustWait:", time.Since(t))

    t = time.Now()
    if err := el.Click(); err != nil {
        fmt.Println("点击失败:", err)
        return
    }
    fmt.Println("Click:", time.Since(t))
    fmt.Println("点击支付宝成功")
}