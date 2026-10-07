package synth

// The schemas follow wp-admin/includes/schema.php as mysqldump prints it.

var postCols = []string{"ID", "post_author", "post_date", "post_date_gmt", "post_content", "post_title",
	"post_excerpt", "post_status", "comment_status", "ping_status", "post_password", "post_name", "to_ping",
	"pinged", "post_modified", "post_modified_gmt", "post_content_filtered", "post_parent", "guid",
	"menu_order", "post_type", "post_mime_type", "comment_count"}

var commentCols = []string{"comment_ID", "comment_post_ID", "comment_author", "comment_author_email",
	"comment_author_url", "comment_author_IP", "comment_date", "comment_date_gmt", "comment_content",
	"comment_karma", "comment_approved", "comment_agent", "comment_type", "comment_parent", "user_id"}

var linkCols = []string{"link_id", "link_url", "link_name", "link_image", "link_target", "link_description",
	"link_visible", "link_owner", "link_rating", "link_updated", "link_rel", "link_notes", "link_rss"}

const schemaPosts = "  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `post_author` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `post_date` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `post_date_gmt` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `post_content` longtext NOT NULL,\n" +
	"  `post_title` text NOT NULL,\n" +
	"  `post_excerpt` text NOT NULL,\n" +
	"  `post_status` varchar(20) NOT NULL DEFAULT 'publish',\n" +
	"  `comment_status` varchar(20) NOT NULL DEFAULT 'open',\n" +
	"  `ping_status` varchar(20) NOT NULL DEFAULT 'open',\n" +
	"  `post_password` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `post_name` varchar(200) NOT NULL DEFAULT '',\n" +
	"  `to_ping` text NOT NULL,\n" +
	"  `pinged` text NOT NULL,\n" +
	"  `post_modified` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `post_modified_gmt` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `post_content_filtered` longtext NOT NULL,\n" +
	"  `post_parent` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `guid` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `menu_order` int(11) NOT NULL DEFAULT 0,\n" +
	"  `post_type` varchar(20) NOT NULL DEFAULT 'post',\n" +
	"  `post_mime_type` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `comment_count` bigint(20) NOT NULL DEFAULT 0,\n" +
	"  PRIMARY KEY (`ID`),\n" +
	"  KEY `post_name` (`post_name`(191)),\n" +
	"  KEY `type_status_date` (`post_type`,`post_status`,`post_date`,`ID`),\n" +
	"  KEY `post_parent` (`post_parent`),\n" +
	"  KEY `post_author` (`post_author`)"

const schemaPostmeta = "  `meta_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `post_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `meta_key` varchar(255) DEFAULT NULL,\n" +
	"  `meta_value` longtext DEFAULT NULL,\n" +
	"  PRIMARY KEY (`meta_id`),\n" +
	"  KEY `post_id` (`post_id`),\n" +
	"  KEY `meta_key` (`meta_key`(191))"

const schemaUsers = "  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `user_login` varchar(60) NOT NULL DEFAULT '',\n" +
	"  `user_pass` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `user_nicename` varchar(50) NOT NULL DEFAULT '',\n" +
	"  `user_email` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `user_url` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `user_registered` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `user_activation_key` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `user_status` int(11) NOT NULL DEFAULT 0,\n" +
	"  `display_name` varchar(250) NOT NULL DEFAULT '',\n" +
	"  PRIMARY KEY (`ID`),\n" +
	"  KEY `user_login_key` (`user_login`),\n" +
	"  KEY `user_nicename` (`user_nicename`),\n" +
	"  KEY `user_email` (`user_email`)"

const schemaUsermeta = "  `umeta_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `user_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `meta_key` varchar(255) DEFAULT NULL,\n" +
	"  `meta_value` longtext DEFAULT NULL,\n" +
	"  PRIMARY KEY (`umeta_id`),\n" +
	"  KEY `user_id` (`user_id`),\n" +
	"  KEY `meta_key` (`meta_key`(191))"

const schemaOptions = "  `option_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `option_name` varchar(191) NOT NULL DEFAULT '',\n" +
	"  `option_value` longtext NOT NULL,\n" +
	"  `autoload` varchar(20) NOT NULL DEFAULT 'yes',\n" +
	"  PRIMARY KEY (`option_id`),\n" +
	"  UNIQUE KEY `option_name` (`option_name`),\n" +
	"  KEY `autoload` (`autoload`)"

const schemaTerms = "  `term_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `name` varchar(200) NOT NULL DEFAULT '',\n" +
	"  `slug` varchar(200) NOT NULL DEFAULT '',\n" +
	"  `term_group` bigint(10) NOT NULL DEFAULT 0,\n" +
	"  PRIMARY KEY (`term_id`),\n" +
	"  KEY `slug` (`slug`(191)),\n" +
	"  KEY `name` (`name`(191))"

const schemaTermTaxonomy = "  `term_taxonomy_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `term_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `taxonomy` varchar(32) NOT NULL DEFAULT '',\n" +
	"  `description` longtext NOT NULL,\n" +
	"  `parent` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `count` bigint(20) NOT NULL DEFAULT 0,\n" +
	"  PRIMARY KEY (`term_taxonomy_id`),\n" +
	"  UNIQUE KEY `term_id_taxonomy` (`term_id`,`taxonomy`),\n" +
	"  KEY `taxonomy` (`taxonomy`)"

const schemaTermRelationships = "  `object_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `term_taxonomy_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `term_order` int(11) NOT NULL DEFAULT 0,\n" +
	"  PRIMARY KEY (`object_id`,`term_taxonomy_id`),\n" +
	"  KEY `term_taxonomy_id` (`term_taxonomy_id`)"

const schemaTermmeta = "  `meta_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `term_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `meta_key` varchar(255) DEFAULT NULL,\n" +
	"  `meta_value` longtext DEFAULT NULL,\n" +
	"  PRIMARY KEY (`meta_id`),\n" +
	"  KEY `term_id` (`term_id`),\n" +
	"  KEY `meta_key` (`meta_key`(191))"

const schemaComments = "  `comment_ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `comment_post_ID` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `comment_author` tinytext NOT NULL,\n" +
	"  `comment_author_email` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `comment_author_url` varchar(200) NOT NULL DEFAULT '',\n" +
	"  `comment_author_IP` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `comment_date` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `comment_date_gmt` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `comment_content` text NOT NULL,\n" +
	"  `comment_karma` int(11) NOT NULL DEFAULT 0,\n" +
	"  `comment_approved` varchar(20) NOT NULL DEFAULT '1',\n" +
	"  `comment_agent` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `comment_type` varchar(20) NOT NULL DEFAULT 'comment',\n" +
	"  `comment_parent` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `user_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  PRIMARY KEY (`comment_ID`),\n" +
	"  KEY `comment_post_ID` (`comment_post_ID`),\n" +
	"  KEY `comment_approved_date_gmt` (`comment_approved`,`comment_date_gmt`),\n" +
	"  KEY `comment_date_gmt` (`comment_date_gmt`),\n" +
	"  KEY `comment_parent` (`comment_parent`),\n" +
	"  KEY `comment_author_email` (`comment_author_email`(10))"

const schemaCommentmeta = "  `meta_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `comment_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `meta_key` varchar(255) DEFAULT NULL,\n" +
	"  `meta_value` longtext DEFAULT NULL,\n" +
	"  PRIMARY KEY (`meta_id`),\n" +
	"  KEY `comment_id` (`comment_id`),\n" +
	"  KEY `meta_key` (`meta_key`(191))"

const schemaLinks = "  `link_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `link_url` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `link_name` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `link_image` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `link_target` varchar(25) NOT NULL DEFAULT '',\n" +
	"  `link_description` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `link_visible` varchar(20) NOT NULL DEFAULT 'Y',\n" +
	"  `link_owner` bigint(20) unsigned NOT NULL DEFAULT 1,\n" +
	"  `link_rating` int(11) NOT NULL DEFAULT 0,\n" +
	"  `link_updated` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `link_rel` varchar(255) NOT NULL DEFAULT '',\n" +
	"  `link_notes` mediumtext NOT NULL,\n" +
	"  `link_rss` varchar(255) NOT NULL DEFAULT '',\n" +
	"  PRIMARY KEY (`link_id`),\n" +
	"  KEY `link_visible` (`link_visible`)"

const schemaBylines = "  `byline_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `post_id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
	"  `byline` varchar(255) NOT NULL DEFAULT '',\n" +
	"  PRIMARY KEY (`byline_id`),\n" +
	"  KEY `post_id` (`post_id`)"

const schemaLog = "  `log_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `message` text NOT NULL,\n" +
	"  `created` datetime NOT NULL,\n" +
	"  PRIMARY KEY (`log_id`)"

const schemaBlogs = "  `blog_id` bigint(20) NOT NULL AUTO_INCREMENT,\n" +
	"  `site_id` bigint(20) NOT NULL DEFAULT 0,\n" +
	"  `domain` varchar(200) NOT NULL DEFAULT '',\n" +
	"  `path` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `registered` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `last_updated` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `public` tinyint(2) NOT NULL DEFAULT 1,\n" +
	"  `archived` tinyint(2) NOT NULL DEFAULT 0,\n" +
	"  `mature` tinyint(2) NOT NULL DEFAULT 0,\n" +
	"  `spam` tinyint(2) NOT NULL DEFAULT 0,\n" +
	"  `deleted` tinyint(2) NOT NULL DEFAULT 0,\n" +
	"  `lang_id` int(11) NOT NULL DEFAULT 0,\n" +
	"  PRIMARY KEY (`blog_id`),\n" +
	"  KEY `domain` (`domain`(50),`path`(5))"

const schemaSite = "  `id` bigint(20) NOT NULL AUTO_INCREMENT,\n" +
	"  `domain` varchar(200) NOT NULL DEFAULT '',\n" +
	"  `path` varchar(100) NOT NULL DEFAULT '',\n" +
	"  PRIMARY KEY (`id`)"

const schemaSitemeta = "  `meta_id` bigint(20) NOT NULL AUTO_INCREMENT,\n" +
	"  `site_id` bigint(20) NOT NULL DEFAULT 0,\n" +
	"  `meta_key` varchar(255) DEFAULT NULL,\n" +
	"  `meta_value` longtext DEFAULT NULL,\n" +
	"  PRIMARY KEY (`meta_id`),\n" +
	"  KEY `meta_key` (`meta_key`(191))"

const schemaSignups = "  `signup_id` bigint(20) NOT NULL AUTO_INCREMENT,\n" +
	"  `domain` varchar(200) NOT NULL DEFAULT '',\n" +
	"  `path` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `title` longtext NOT NULL,\n" +
	"  `user_login` varchar(60) NOT NULL DEFAULT '',\n" +
	"  `user_email` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `registered` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `activated` datetime NOT NULL DEFAULT '0000-00-00 00:00:00',\n" +
	"  `active` tinyint(1) NOT NULL DEFAULT 0,\n" +
	"  `activation_key` varchar(50) NOT NULL DEFAULT '',\n" +
	"  `meta` longtext DEFAULT NULL,\n" +
	"  PRIMARY KEY (`signup_id`)"

const schemaSubscribers = "  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `subscriber_email` varchar(100) NOT NULL DEFAULT '',\n" +
	"  `signup_ip` varchar(45) NOT NULL DEFAULT '',\n" +
	"  PRIMARY KEY (`id`)"
