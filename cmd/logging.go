package cmd

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	log "github.com/sirupsen/logrus"
)

type plainTextFormatter struct{}

func (*plainTextFormatter) Format(entry *log.Entry) ([]byte, error) {
	buffer := entry.Buffer
	if buffer == nil {
		buffer = new(bytes.Buffer)
	}
	level := entry.Level.String()
	level = strings.ToUpper(level[:1]) + level[1:]
	message := strings.TrimRight(entry.Message, "\r\n")
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", `\r`), "\n", `\n`)
	fmt.Fprintf(buffer, "%s [%s] %s", entry.Time.Format("2006/01/02 15:04:05"), level, message)
	host, hasHost := entry.Data["Host"]
	id, hasID := entry.Data["ID"]
	nodeType, hasType := entry.Data["Type"]
	compactNode := hasHost && hasID && hasType
	if compactNode {
		node := fmt.Sprintf(" %v|%v|%v", host, id, nodeType)
		buffer.WriteString(strings.ReplaceAll(strings.ReplaceAll(node, "\r", `\r`), "\n", `\n`))
	}
	keys := make([]string, 0, len(entry.Data))
	for key := range entry.Data {
		if compactNode && (key == "Host" || key == "ID" || key == "Type") {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		switch value := entry.Data[key].(type) {
		case string:
			fmt.Fprintf(buffer, " %s=%q", key, value)
		default:
			fmt.Fprintf(buffer, " %s=%v", key, value)
		}
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}

func init() {
	log.SetFormatter(&plainTextFormatter{})
}
