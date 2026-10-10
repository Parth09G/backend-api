-- Objects Supabase provides that the migrations reference. Used only by sqlc
-- for type-checking; never applied to a database.
create schema if not exists auth;
create table if not exists auth.users (id uuid primary key, email text);
create schema if not exists extensions;
