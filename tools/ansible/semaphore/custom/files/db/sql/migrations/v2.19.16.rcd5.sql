create table rcd_application (
 project_id integer not null references project(id) on delete cascade,
 component varchar(32) not null,
 body text not null,
 created_at varchar(40) not null,
 created_by varchar(255) not null,
 primary key(project_id,component)
);
