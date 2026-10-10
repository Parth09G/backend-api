-- Development-only sample data (applied by `supabase db reset` on the local stack).
-- Never run against production. Locations and units come from the first migration.

SET search_path = app, extensions;

INSERT INTO shop_settings (id, shop_name, address, state_code, gstin, phone, invoice_terms, number_prefixes)
VALUES (1, 'Demo Building Materials', '1 Market Road, Bengaluru 560001', '29', '29ABCDE1234F1Z5', '9800000000',
        'Goods once sold will not be taken back without approval.', '{}'::jsonb);

INSERT INTO category (name, default_hsn, default_gst) VALUES
  ('Cement',        '2523', 18),
  ('Steel',         '7214', 18),
  ('Nails',         '7317', 18),
  ('Waterproofing', '3824', 18),
  ('Steel pipes',   '7306', 18);

INSERT INTO category_unit (category_id, unit_id, is_default)
SELECT c.id, u.id, v.is_default
  FROM (VALUES ('Cement', 'bag', true), ('Steel', 'kg', true), ('Steel', 'ton', false), ('Steel', 'bdl', false),
               ('Nails', 'kg', true), ('Waterproofing', 'ltr', true), ('Steel pipes', 'nos', true),
               ('Steel pipes', 'kg', false)) AS v(category, symbol, is_default)
  JOIN category c ON c.name = v.category
  JOIN unit u ON u.symbol = v.symbol;

INSERT INTO category_attribute (category_id, code, label, data_type, options, value_unit, is_required, sort_order)
SELECT c.id, a.code, a.label, a.data_type::attr_type, a.options, a.value_unit, a.is_required, a.sort_order
  FROM (VALUES
    ('Cement',        'brand',     'Brand',     'text',   NULL::text[],                          NULL,   true,  1),
    ('Cement',        'grade',     'Grade',     'select', ARRAY['OPC 43','OPC 53','PPC'],        NULL,   true,  2),
    ('Steel',         'brand',     'Brand',     'text',   NULL,                                  NULL,   true,  1),
    ('Steel',         'size',      'Size',      'number', NULL,                                  'mm',   true,  2),
    ('Nails',         'size',      'Size',      'text',   NULL,                                  NULL,   true,  1),
    ('Waterproofing', 'brand',     'Brand',     'text',   NULL,                                  NULL,   true,  1),
    ('Waterproofing', 'pack_size', 'Pack size', 'number', NULL,                                  'L',    true,  2),
    ('Steel pipes',   'size',      'Size',      'text',   NULL,                                  NULL,   true,  1),
    ('Steel pipes',   'shape',     'Shape',     'select', ARRAY['Round','Square','Rectangular'], NULL,   true,  2)
  ) AS a(category, code, label, data_type, options, value_unit, is_required, sort_order)
  JOIN category c ON c.name = a.category;

INSERT INTO item (category_id, name, attributes, hsn_code, gst_rate, base_unit_id, reorder_level)
SELECT c.id, v.name, v.attrs::jsonb, c.default_hsn, c.default_gst, u.id, v.reorder
  FROM (VALUES
    ('Cement',        'UltraTech OPC 53',      '{"brand":"UltraTech","grade":"OPC 53"}', 'bag', 50),
    ('Cement',        'ACC PPC',               '{"brand":"ACC","grade":"PPC"}',          'bag', 50),
    ('Steel',         'JSW 8 mm',              '{"brand":"JSW","size":8}',               'kg',  500),
    ('Steel',         'JSW 12 mm',             '{"brand":"JSW","size":12}',              'kg',  500),
    ('Nails',         '2 inch',                '{"size":"2 inch"}',                      'kg',  20),
    ('Waterproofing', 'Dr Fixit 1',            '{"brand":"Dr Fixit","pack_size":1}',     'ltr', 10),
    ('Steel pipes',   '1 inch Round',          '{"size":"1 inch","shape":"Round"}',      'nos', 20)
  ) AS v(category, name, attrs, unit, reorder)
  JOIN category c ON c.name = v.category
  JOIN unit u ON u.symbol = v.unit;

-- Every item can be sold in its base unit; steel also in tonnes (1 ton = 1000 kg).
INSERT INTO item_unit (item_id, unit_id, factor_to_base)
SELECT id, base_unit_id, 1 FROM item;
INSERT INTO item_unit (item_id, unit_id, factor_to_base)
SELECT i.id, u.id, 1000 FROM item i JOIN category c ON c.id = i.category_id, unit u
 WHERE c.name = 'Steel' AND u.symbol = 'ton';

-- Opening stock in Godown 1 at a sample cost.
SELECT fn_stock_in(i.id, l.id, 200, 350, 'opening', NULL, NULL, NULL, NULL)
  FROM item i, location l WHERE l.name = 'Godown 1';

INSERT INTO party (kind, name, mobile, gstin, state_code, billing_address, credit_limit) VALUES
  ('customer', 'Sharma Constructions',   '9800000001', '29AAAPS1234C1Z5', '29', 'Whitefield, Bengaluru', 200000),
  ('customer', 'Ravi Kumar',             '9800000002', NULL,              '29', 'Jayanagar, Bengaluru',  50000),
  ('supplier', 'Southern Cement Agency', '9800000003', '29AABCS5678D1Z2', '29', 'Peenya, Bengaluru',     NULL);
