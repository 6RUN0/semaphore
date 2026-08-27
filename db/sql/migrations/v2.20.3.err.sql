{{if .Mysql}}drop index `task__status` on `task`;{{else}}drop index if exists `task__status`;{{end}}
