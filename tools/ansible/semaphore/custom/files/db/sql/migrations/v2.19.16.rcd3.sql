create table rcd_operation (
 project_id integer not null references project(id) on delete cascade,
 id varchar(64) not null,
 request_key varchar(128) not null,
 request_hash varchar(64) not null,
 nonce varchar(64) not null,
 body text not null,
 state varchar(40) not null,
 revision integer not null,
 task_id integer not null,
 created_at varchar(40) not null,
 primary key(project_id,id),
 unique(project_id,request_key)
);
create table rcd_operation_lock (
 project_id integer not null references project(id) on delete cascade,
 device_id varchar(64) not null,
 operation_id varchar(64) not null,
 primary key(project_id,device_id)
);
