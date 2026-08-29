-- "if not exists" on every statement because this migration creates four
-- tables and ApplyMigration cannot undo a partial run: a failing statement ends
-- the process (log.Fatal), so the rollback file never runs, and on MySQL the
-- DDL that already succeeded is committed anyway. Without it, a failure on the
-- third table leaves the first two behind, the migrations row unwritten, and
-- every later start failing on "table already exists" until somebody drops them
-- by hand.
create table if not exists `project__inventory_snapshot` (
  `inventory_id` int not null primary key,
  `project_id` int not null,
  `status` varchar(20) not null,
  `error` varchar(1000) not null default '',
  `source_hash` varchar(64) not null,
  `host_count` int not null default 0,
  `created` datetime not null,
  `task_id` int,
  foreign key (`inventory_id`) references `project__inventory`(`id`) on delete cascade,
  foreign key (`project_id`) references `project`(`id`) on delete cascade,
  foreign key (`task_id`) references `task`(`id`) on delete set null
);

create table if not exists `project__inventory_snapshot_host` (
  `inventory_id` int not null,
  `name` varchar(255){{if .Mysql}} character set utf8mb4 collate utf8mb4_bin{{end}} not null,
  primary key (`inventory_id`, `name`),
  foreign key (`inventory_id`) references `project__inventory`(`id`) on delete cascade
);

create table if not exists `project__inventory_snapshot_group` (
  `inventory_id` int not null,
  `name` varchar(255){{if .Mysql}} character set utf8mb4 collate utf8mb4_bin{{end}} not null,
  `host_count` int not null default 0,
  primary key (`inventory_id`, `name`),
  foreign key (`inventory_id`) references `project__inventory`(`id`) on delete cascade
);

create table if not exists `project__inventory_snapshot_host_group` (
  `inventory_id` int not null,
  `group_name` varchar(255){{if .Mysql}} character set utf8mb4 collate utf8mb4_bin{{end}} not null,
  `host_name` varchar(255){{if .Mysql}} character set utf8mb4 collate utf8mb4_bin{{end}} not null,
  primary key (`inventory_id`, `group_name`, `host_name`),
  foreign key (`inventory_id`) references `project__inventory`(`id`) on delete cascade
);
