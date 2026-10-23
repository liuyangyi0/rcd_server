create table rcd_device (
 project_id integer not null references project(id) on delete cascade,
 id varchar(64) not null,
 template_id integer not null,
 inventory_id integer not null,
 host_alias varchar(128) not null,
 binding text not null,
 revision integer not null,
 created_by varchar(255) not null,
 last_task_id integer not null,
 last_attempt_at varchar(40) not null,
 last_success_at varchar(40) not null,
 last_error text not null,
 components text not null,
 primary key(project_id,id),
 unique(project_id,template_id),
 unique(project_id,inventory_id,host_alias)
);
