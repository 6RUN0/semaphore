{{if .Mysql}}drop index `project__inventory_snapshot__task` on `project__inventory_snapshot`;{{else}}drop index if exists `project__inventory_snapshot__task`;{{end}}
