BEGIN;

DROP TABLE IF EXISTS order_items CASCADE;
DROP TABLE IF EXISTS orders CASCADE;
DROP TABLE IF EXISTS users CASCADE;
DROP TABLE IF EXISTS articles CASCADE;

CREATE TABLE users (
    user_id SERIAL PRIMARY KEY,
    first_name VARCHAR(50),
    last_name VARCHAR(50),
    email VARCHAR(100) UNIQUE,
    signup_date DATE NOT NULL
);

CREATE TABLE articles (
    article_id SERIAL PRIMARY KEY,
    article_name VARCHAR(100),
    category VARCHAR(50),
    price NUMERIC(10,2),
    cost NUMERIC(10,2),
    created_at DATE
);

CREATE TABLE orders (
    order_id SERIAL PRIMARY KEY,
    user_id INT REFERENCES users(user_id),
    order_date DATE NOT NULL,
    status VARCHAR(20) CHECK (status IN ('completed','cancelled','pending')),
    total_amount NUMERIC(10,2)
);

CREATE TABLE order_items (
    order_item_id SERIAL PRIMARY KEY,
    order_id INT REFERENCES orders(order_id) ON DELETE CASCADE,
    article_id INT REFERENCES articles(article_id),
    quantity INT CHECK (quantity > 0),
    unit_price NUMERIC(10,2)
);

INSERT INTO users (first_name, last_name, email, signup_date) VALUES
('Alice', 'Smith', 'alice@example.com', '2023-01-15'),
('Bob', 'Müller', 'bob@example.com', '2023-03-22'),
('Carlos', 'Garcia', 'carlos@example.com', '2023-05-10'),
('Yuki', 'Tanaka', 'yuki@example.com', '2023-06-05'),
('Emma', 'Johnson', 'emma@example.com', '2023-07-18');

INSERT INTO articles (article_name, category, price, cost, created_at) VALUES
('Laptop Pro 15', 'Electronics', 1500.00, 1000.00, '2023-01-01'),
('Wireless Mouse', 'Electronics', 40.00, 15.00, '2023-01-10'),
('Office Chair', 'Furniture', 250.00, 120.00, '2023-02-01'),
('Standing Desk', 'Furniture', 600.00, 350.00, '2023-02-15'),
('Noise Cancelling Headphones', 'Electronics', 300.00, 180.00, '2023-03-01');

INSERT INTO orders (user_id, order_date, status, total_amount) VALUES
(1, '2023-08-01', 'completed', 1540.00),
(2, '2023-08-03', 'completed', 250.00),
(3, '2023-08-10', 'completed', 600.00),
(1, '2023-09-05', 'cancelled', 40.00),
(4, '2023-09-12', 'completed', 300.00),
(5, '2023-10-01', 'completed', 1540.00);

INSERT INTO order_items (order_id, article_id, quantity, unit_price) VALUES
(1, 1, 1, 1500.00),
(1, 2, 1, 40.00),
(2, 3, 1, 250.00),
(3, 4, 1, 600.00),
(4, 2, 1, 40.00),
(5, 5, 1, 300.00),
(6, 1, 1, 1500.00),
(6, 2, 1, 40.00);

CREATE INDEX idx_orders_user ON orders(user_id);
CREATE INDEX idx_orders_date ON orders(order_date);
CREATE INDEX idx_order_items_order ON order_items(order_id);
CREATE INDEX idx_order_items_article ON order_items(article_id);

COMMIT;